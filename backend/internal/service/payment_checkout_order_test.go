//go:build unit

package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestCheckoutPostgresAdmissionAndReplayConcurrent(t *testing.T) {
	dsn := os.Getenv("CHECKOUT_TEST_DSN")
	if dsn == "" {
		t.Skip("set CHECKOUT_TEST_DSN to an isolated PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer base.Close()
	schema := fmt.Sprintf("checkout_%d", time.Now().UnixNano())
	_, err = base.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	defer func() { _, _ = base.Exec("DROP SCHEMA " + schema + " CASCADE") }()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	require.NoError(t, client.Schema.Create(ctx))
	owner, err := client.User.Create().SetEmail("concurrent@example.test").SetUsername("buyer").SetPasswordHash("hash").Save(ctx)
	require.NoError(t, err)
	cfg := NewPaymentConfigService(client, &paymentConfigSettingRepoStub{values: map[string]string{
		SettingPaymentEnabled: "true", SettingMaxPendingOrders: "1",
	}}, nil)
	svc := NewPaymentService(client, payment.NewRegistry(), payment.NewDefaultLoadBalancer(client, nil), nil, nil, cfg, nil, nil, nil)
	svc.resumeService = NewPaymentResumeService([]byte("postgres-checkout.test"))
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"code":1,"qrcode":"example-qr"}`))
	}))
	defer upstream.Close()
	addCheckoutEasyPay(t, svc, upstream.URL, "", "")
	create := CheckoutCreateRequest{Amount: 20, IdempotencyKey: "concurrent-key"}
	type result struct {
		id  int64
		err error
	}
	const clients = 8
	created := make(chan result, clients)
	for range clients {
		go func() {
			order, err := svc.CreateCheckoutOrder(ctx, owner.ID, create)
			if err != nil {
				created <- result{err: err}
				return
			}
			created <- result{id: order.ID}
		}()
	}
	var orderID int64
	for range clients {
		r := <-created
		require.NoError(t, r.err)
		if orderID == 0 {
			orderID = r.id
		}
		require.Equal(t, orderID, r.id)
	}
	order, err := svc.GetCheckoutOrder(ctx, orderID, owner.ID)
	require.NoError(t, err)
	quote, err := svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	gate := make(chan struct{})
	joined := make(chan error, clients+1)
	for range clients {
		go func() {
			<-gate
			_, err := svc.PayCheckoutOrder(ctx, order, CheckoutPayRequest{
				PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test",
			})
			joined <- err
		}()
	}
	go func() {
		<-gate
		_, err := svc.createOrderInTx(ctx, CreateOrderRequest{UserID: owner.ID, OrderType: payment.OrderTypeBalance},
			&User{Email: owner.Email, Username: owner.Username}, nil, &PaymentConfig{MaxPendingOrders: 1}, 12, 12, 0, 12, 0, nil)
		joined <- err
	}()
	close(gate)
	var successes int
	for range clients + 1 {
		err := <-joined
		if err == nil {
			successes++
		} else {
			require.Equal(t, "TOO_MANY_PENDING", infraerrors.Reason(err), "%v", err)
		}
	}
	count, err := client.PaymentOrder.Query().Where(paymentorder.StatusEQ(OrderStatusPending)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	if upstreamCalls.Load() == 0 {
		require.Equal(t, 1, successes, "legacy won; all checkout attempts must be rejected")
	} else {
		require.EqualValues(t, 1, upstreamCalls.Load())
		require.Equal(t, clients, successes, "checkout won; all retries should replay and legacy should be rejected")
	}
	// Verify the persisted recovery cursor can advance repeatedly in PostgreSQL
	// without touching the fulfillment lease's updated_at value.
	order, err = client.PaymentOrder.UpdateOneID(orderID).SetCheckoutStartedAt(time.Now().Add(-time.Hour)).
		SetStatus(OrderStatusExpired).Save(ctx)
	require.NoError(t, err)
	for range 2 {
		_, err = svc.reconcileRecentCheckoutOrders(ctx)
		require.NoError(t, err)
		current, err := client.PaymentOrder.Get(ctx, orderID)
		require.NoError(t, err)
		require.NotNil(t, current.CheckoutReconciledAt)
		if order.CheckoutReconciledAt != nil {
			require.True(t, current.CheckoutReconciledAt.After(*order.CheckoutReconciledAt))
		}
		require.WithinDuration(t, order.UpdatedAt, current.UpdatedAt, time.Microsecond)
		order = current
	}
}

func newCheckoutOrderTestService(t *testing.T, settings map[string]string) (*PaymentService, *dbent.User) {
	t.Helper()
	client := newPaymentConfigServiceTestClient(t)
	user, err := client.User.Create().SetEmail("checkout@example.test").SetUsername("buyer").SetPasswordHash("hash").Save(context.Background())
	require.NoError(t, err)
	settings[SettingPaymentEnabled] = "true"
	config := NewPaymentConfigService(client, &paymentConfigSettingRepoStub{values: settings}, nil)
	svc := NewPaymentService(client, payment.NewRegistry(), payment.NewDefaultLoadBalancer(client, nil), nil, nil, config, nil, nil, nil)
	svc.resumeService = NewPaymentResumeService([]byte("checkout-test-key"))
	return svc, user
}

func addCheckoutEasyPay(t *testing.T, svc *PaymentService, endpoint, mode, limits string) {
	t.Helper()
	config, err := json.Marshal(map[string]string{
		"pid": "merchant", "pkey": "test-secret", "apiBase": endpoint,
		"notifyUrl": "https://app.example.test/notify", "returnUrl": "https://app.example.test/payment/result",
	})
	require.NoError(t, err)
	_, err = svc.entClient.PaymentProviderInstance.Create().SetProviderKey(payment.TypeEasyPay).SetSupportedTypes("alipay").
		SetPaymentMode(mode).SetConfig(string(config)).SetLimits(limits).Save(context.Background())
	require.NoError(t, err)
}

func paidCheckoutRedeemStub(order *dbent.PaymentOrder) *RedeemService {
	// Simulate an already-applied redeem code so the real completion path can
	// run without needing the separate user balance repository in this fixture.
	return &RedeemService{redeemRepo: &paymentOrderLifecycleRedeemRepo{codesByCode: map[string]*RedeemCode{
		order.RechargeCode: {ID: 42, Code: order.RechargeCode, Type: RedeemTypeBalance, Value: order.Amount, Status: StatusUsed, UsedBy: &order.UserID},
	}}}
}

func TestCheckoutCreateIdempotentAndHiddenUntilPay(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{SettingMaxPendingOrders: "1"})
	legacy, err := svc.createOrderInTx(ctx, CreateOrderRequest{UserID: user.ID, OrderType: payment.OrderTypeBalance},
		&User{Email: user.Email, Username: user.Username}, nil, &PaymentConfig{MaxPendingOrders: 1}, 12, 12, 0, 12, 0, nil)
	require.NoError(t, err)
	input := CheckoutCreateRequest{Amount: 20, IdempotencyKey: "one"}
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, input)
	require.NoError(t, err, "unconfirmed checkout must not consume a legacy pending slot")
	require.Equal(t, OrderStatusAwaitingConfirmation, order.Status)
	require.Regexp(t, `^[0-9a-f]{32}$`, order.OutTradeNo)
	require.Nil(t, order.CheckoutStartedAt)
	replay, err := svc.CreateCheckoutOrder(ctx, user.ID, input)
	require.NoError(t, err)
	require.Equal(t, order.ID, replay.ID)
	input.Amount++
	_, err = svc.CreateCheckoutOrder(ctx, user.ID, input)
	require.Equal(t, "IDEMPOTENCY_KEY_CONFLICT", infraerrors.Reason(err))
	_, err = svc.GetCheckoutOrder(ctx, order.ID, user.ID+100)
	require.True(t, infraerrors.IsNotFound(err))
	_, err = svc.GetCheckoutOrder(ctx, legacy.ID, user.ID)
	require.True(t, infraerrors.IsNotFound(err))
	listed, count, err := svc.GetUserOrders(ctx, user.ID, OrderListParams{})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, legacy.ID, listed[0].ID)
	listed, count, err = svc.AdminListOrders(ctx, user.ID, OrderListParams{})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, legacy.ID, listed[0].ID)

	addCheckoutEasyPay(t, svc, "http://127.0.0.1:1", "", "")
	quote, err := svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	_, err = svc.PayCheckoutOrder(ctx, order, CheckoutPayRequest{PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test"})
	require.Equal(t, "TOO_MANY_PENDING", infraerrors.Reason(err))
	_, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetExpiresAt(time.Now().Add(-time.Minute)).Save(ctx)
	require.NoError(t, err)
	expired, err := svc.ExpireTimedOutOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, expired)
	order, err = svc.GetCheckoutOrder(ctx, order.ID, user.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusExpired, order.Status)
	err = svc.HandlePaymentNotification(ctx, &payment.PaymentNotification{OrderID: fmt.Sprintf("sub2_%d", order.ID),
		Amount: 20, Status: payment.NotificationStatusSuccess}, payment.TypeEasyPay)
	require.ErrorIs(t, err, ErrOrderNotFound)
}

func TestCheckoutCancelRequiresActionTokenAndUnstartedOrder(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{})
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "cancel"})
	require.NoError(t, err)
	other, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 30, IdempotencyKey: "other"})
	require.NoError(t, err)
	token := svc.IssueCheckoutActionToken(order)
	for _, invalid := range []string{"", svc.IssueCheckoutActionToken(other)} {
		require.ErrorContains(t, svc.CancelUnstartedCheckout(ctx, order, invalid), "CHECKOUT_CANCEL_UNAVAILABLE")
	}
	require.True(t, svc.CanCancelUnstartedCheckout(order, token))
	require.NoError(t, svc.CancelUnstartedCheckout(ctx, order, token))
	current, err := svc.GetPublicCheckoutOrder(ctx, order.OutTradeNo)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCancelled, current.Status)
	require.False(t, svc.CanCancelUnstartedCheckout(current, token))
	require.ErrorContains(t, svc.CancelUnstartedCheckout(ctx, order, token), "CHECKOUT_NOT_PAYABLE")
	_, err = svc.PayCheckoutOrder(ctx, order, CheckoutPayRequest{SiteOrigin: "https://app.example.test"})
	require.ErrorContains(t, err, "CHECKOUT_NOT_PAYABLE")
	started, err := svc.entClient.PaymentOrder.UpdateOneID(other.ID).SetStatus(OrderStatusPending).SetCheckoutStartedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	require.False(t, svc.CanCancelUnstartedCheckout(started, svc.IssueCheckoutActionToken(other)))
	require.ErrorContains(t, svc.CancelUnstartedCheckout(ctx, other, svc.IssueCheckoutActionToken(other)), "CHECKOUT_NOT_PAYABLE")
}

func TestCheckoutSwitchRequiresOriginalSessionAndVerifiedClose(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{})
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "original"})
	require.NoError(t, err)
	inst, err := svc.entClient.PaymentProviderInstance.Create().SetProviderKey(payment.TypeWxpay).SetSupportedTypes("wxpay").SetConfig("{}").Save(ctx)
	require.NoError(t, err)
	token := svc.IssueCheckoutActionToken(order)
	require.NotEmpty(t, token)
	order, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusPending).SetCheckoutStartedAt(time.Now().Add(-time.Minute)).
		SetProviderKey(payment.TypeWxpay).SetProviderInstanceID(fmt.Sprint(inst.ID)).SetPaymentType(payment.TypeWxpay).
		SetAmount(35).SetProviderSnapshot(map[string]any{"checkout_input_amount": 20.0}).Save(ctx)
	require.NoError(t, err)
	prov := &paymentOrderLifecycleQueryProvider{key: payment.TypeWxpay, resp: &payment.QueryOrderResponse{Status: payment.ProviderStatusFailed}}
	defer replacePaymentProviderFactoryForTest(t, prov)()
	other, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "other"})
	require.NoError(t, err)
	wrongUser := *order
	wrongUser.UserID++
	expired := *order
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	deadline := fmt.Sprint(time.Now().Add(-time.Minute).Unix())
	expiredToken := deadline + "." + svc.paymentResume().sign(checkoutActionPayload(order, deadline))
	require.False(t, svc.CanSwitchCheckoutMethod(&wrongUser, token))
	require.False(t, svc.CanSwitchCheckoutMethod(&expired, token))
	for _, bad := range []string{"", strings.Repeat("b", 64), expiredToken, svc.IssueCheckoutActionToken(other)} {
		_, err = svc.SwitchCheckoutMethod(ctx, order, bad)
		require.ErrorContains(t, err, "CHECKOUT_SWITCH_UNAVAILABLE")
	}
	require.Zero(t, prov.queryCalls)
	_, err = svc.SwitchCheckoutMethod(ctx, order, token)
	require.ErrorContains(t, err, "CHECKOUT_CANCEL_UNCONFIRMED")
	require.Zero(t, prov.cancelCalls)
	unchanged, err := svc.GetPublicCheckoutOrder(ctx, order.OutTradeNo)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, unchanged.Status)
	prov.resp.Status = payment.ProviderStatusPending
	replacement, err := svc.SwitchCheckoutMethod(ctx, order, token)
	require.NoError(t, err)
	require.Equal(t, order.OutTradeNo, prov.lastCancelTradeNo)
	require.NotEqual(t, order.OutTradeNo, replacement.OutTradeNo)
	require.Equal(t, order.UserID, replacement.UserID)
	require.Equal(t, 20.0, replacement.Amount)
	require.Equal(t, OrderStatusAwaitingConfirmation, replacement.Status)
	require.Nil(t, replacement.CheckoutStartedAt)
	owned, err := svc.GetCheckoutOrderByNumber(ctx, order.OutTradeNo, user.ID)
	require.NoError(t, err)
	require.Equal(t, order.OutTradeNo, owned.OutTradeNo)
	require.Equal(t, OrderStatusCancelled, owned.Status)
	replacementID, err := svc.OwnedCheckoutReplacementID(ctx, owned, user.ID)
	require.NoError(t, err)
	require.Equal(t, replacement.OutTradeNo, replacementID)
	replacementID, err = svc.OwnedCheckoutReplacementID(ctx, owned, user.ID+1)
	require.NoError(t, err)
	require.Empty(t, replacementID)
	_, err = svc.GetCheckoutOrderByNumber(ctx, order.OutTradeNo, user.ID+1)
	require.Equal(t, "NOT_FOUND", infraerrors.Reason(err))
	public, err := svc.GetPublicCheckoutOrder(ctx, order.OutTradeNo)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCancelled, public.Status)
	require.Equal(t, order.OutTradeNo, public.OutTradeNo)
	_, err = svc.SwitchCheckoutMethod(ctx, order, token)
	require.ErrorContains(t, err, "CHECKOUT_SWITCH_UNAVAILABLE")
	require.Equal(t, 1, prov.cancelCalls)
}

func TestCheckoutPayReservesOnceAndReplaysPersistedLaunch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc, user := newCheckoutOrderTestService(t, map[string]string{SettingRechargeFeeRate: "2", SettingBalanceRechargeMult: "3"})
	entered := make(chan string, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- r.FormValue("return_url")
		select {
		case <-release:
		case <-ctx.Done():
		}
		_, _ = w.Write([]byte(`{"code":1,"trade_no":"preliminary","qrcode":"https://gateway.example.test/pay"}`))
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	addCheckoutEasyPay(t, svc, upstream.URL, "", "")
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "started"})
	require.NoError(t, err)
	methods, err := svc.GetCheckoutMethods(ctx, order)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	quote, err := svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	require.Equal(t, 60.0, quote.Amount)
	require.Equal(t, 20.4, quote.PayAmount)
	input := CheckoutPayRequest{PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test"}
	type result struct {
		response *CheckoutPayResponse
		err      error
	}
	first := make(chan result, 1)
	go func() {
		resp, err := svc.PayCheckoutOrder(ctx, order, input)
		first <- result{resp, err}
	}()
	select {
	case returnURL := <-entered:
		require.Contains(t, returnURL, "https://app.example.test/payment/checkout/"+order.OutTradeNo)
		require.NotContains(t, returnURL, "#")
	case <-ctx.Done():
		t.Fatal("upstream create was not called")
	}
	replayReq := input
	replayReq.QuoteID = ""
	starting, err := svc.PayCheckoutOrder(ctx, order, replayReq)
	require.NoError(t, err)
	require.Equal(t, order.ID, starting.OrderID)
	require.Empty(t, starting.QRCode)
	require.Empty(t, starting.ResumeToken)
	close(release)
	res := <-first
	require.NoError(t, res.err)
	require.Equal(t, "https://gateway.example.test/pay", res.response.QRCode)
	replay, err := svc.PayCheckoutOrder(ctx, order, replayReq)
	require.NoError(t, err)
	require.Equal(t, res.response.QRCode, replay.QRCode)
	require.Equal(t, order.OutTradeNo, starting.OutTradeNo)
	require.Equal(t, starting.OutTradeNo, replay.OutTradeNo)
	require.EqualValues(t, 1, calls.Load())
	orders, count, err := svc.AdminListOrders(ctx, user.ID, OrderListParams{})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, order.ID, orders[0].ID)
	_, err = svc.createOrderInTx(ctx, CreateOrderRequest{UserID: user.ID, OrderType: payment.OrderTypeBalance},
		&User{Email: user.Email, Username: user.Username}, nil, &PaymentConfig{MaxPendingOrders: 1}, 12, 12, 0, 12, 0, nil)
	require.Equal(t, "TOO_MANY_PENDING", infraerrors.Reason(err), "legacy creation and checkout share the pending limit")
}

func TestCheckoutLostCreateResponseRecoversByMerchantOrder(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{})
	var creates, queries atomic.Int32
	var paid atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mapi.php":
			creates.Add(1)
			_, _ = w.Write([]byte(`invalid create response`))
		case "/api.php":
			queries.Add(1)
			if !paid.Load() {
				_, _ = w.Write([]byte(`{"code":1,"status":0}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"code":1,"status":1,"money":"20.00","trade_no":"confirmed-trade","out_trade_no":%q}`, r.FormValue("out_trade_no"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	addCheckoutEasyPay(t, svc, upstream.URL, "", "")
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "uncertain"})
	require.NoError(t, err)
	quote, err := svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	input := CheckoutPayRequest{PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test"}
	resp, err := svc.PayCheckoutOrder(ctx, order, input)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, resp.Status)
	require.Empty(t, resp.QRCode)
	require.Empty(t, resp.ResumeToken)
	started, err := svc.GetCheckoutOrder(ctx, order.ID, user.ID)
	require.NoError(t, err)
	_, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetCheckoutStartedAt(time.Now().Add(-time.Hour)).SetExpiresAt(time.Now().Add(-time.Minute)).Save(ctx)
	require.NoError(t, err)
	_, err = svc.CancelOrder(ctx, order.ID, user.ID)
	require.Equal(t, "CHECKOUT_CANCEL_UNCONFIRMED", infraerrors.Reason(err))
	count, err := svc.ExpireTimedOutOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetUpdatedAt(time.Now().Add(-time.Hour)).Save(ctx)
	require.NoError(t, err)
	// A fresh service (like a process restart) can recover after local expiry,
	// including long after the original legacy webhook grace period.
	resumedSvc := NewPaymentService(svc.entClient, payment.NewRegistry(), payment.NewDefaultLoadBalancer(svc.entClient, nil),
		paidCheckoutRedeemStub(started), nil, svc.configService, nil, nil, nil)
	resumedSvc.resumeService = svc.resumeService
	paid.Store(true)
	// The background sweep recovers even if neither the webhook nor the browser returns.
	count, err = resumedSvc.reconcileRecentCheckoutOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	current, err := resumedSvc.GetPublicCheckoutOrder(ctx, order.OutTradeNo)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, current.Status)
	require.Equal(t, "confirmed-trade", current.PaymentTradeNo)
	err = resumedSvc.persistCheckoutLaunch(ctx, started, &payment.CreatePaymentResponse{TradeNo: "stale-trade", QRCode: "stale-qr"}, nil)
	require.NoError(t, err)
	current, err = resumedSvc.GetCheckoutOrder(ctx, order.ID, user.ID)
	require.NoError(t, err)
	require.Equal(t, "confirmed-trade", current.PaymentTradeNo)
	require.Nil(t, current.QrCode)
	replay, err := resumedSvc.PayCheckoutOrder(ctx, order, input)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, replay.Status)
	require.EqualValues(t, 1, creates.Load())
	require.GreaterOrEqual(t, queries.Load(), int32(3))
}

func TestCheckoutConfirmationRequiresExactChargeAndOfficialWeChatIdentity(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{})
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "callback"})
	require.NoError(t, err)
	order, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusPending).
		SetCheckoutStartedAt(time.Now()).SetPayAmount(20).SetOutTradeNo("sub2_checkout_callback").
		SetRechargeCode("checkout-redeem").SetPaymentType(payment.TypeWxpay).SetProviderKey(payment.TypeWxpay).
		SetProviderSnapshot(map[string]any{"provider_key": "wxpay", "merchant_app_id": "app", "merchant_id": "mch", "currency": "CNY"}).Save(ctx)
	require.NoError(t, err)
	svc.redeemService = paidCheckoutRedeemStub(order)
	n := &payment.PaymentNotification{OrderID: order.OutTradeNo, TradeNo: "upstream-trade", Amount: 20, Status: payment.NotificationStatusSuccess}
	err = svc.HandlePaymentNotification(ctx, n, payment.TypeWxpay)
	require.ErrorContains(t, err, "merchant or currency evidence")
	n.Metadata = map[string]string{"appid": "app", "mchid": "mch", "currency": "CNY", "trade_state": "SUCCESS"}
	n.Amount = 19.99
	err = svc.HandlePaymentNotification(ctx, n, payment.TypeWxpay)
	require.ErrorContains(t, err, "amount mismatch")
	n.Amount = 20
	require.NoError(t, svc.HandlePaymentNotification(ctx, n, payment.TypeWxpay))
	require.NoError(t, svc.persistCheckoutLaunch(ctx, order, &payment.CreatePaymentResponse{TradeNo: "stale"}, nil))
	current, err := svc.entClient.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, current.Status)
	require.Equal(t, "upstream-trade", current.PaymentTradeNo)
}

func TestCheckoutRecoveryCompletesPersistedPaidOrderWithoutProvider(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{})
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "paid-crash"})
	require.NoError(t, err)
	olderThanWindow := time.Now().Add(-checkoutPaymentRecoveryWindow - time.Hour)
	order, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusPaid).
		SetCheckoutStartedAt(olderThanWindow).SetPaidAt(time.Now()).SetPayAmount(20).SetRechargeCode("already-redeemed").Save(ctx)
	require.NoError(t, err)
	svc.redeemService = paidCheckoutRedeemStub(order)
	_, err = svc.reconcileRecentCheckoutOrders(ctx)
	require.NoError(t, err)
	current, err := svc.entClient.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPaid, current.Status, "checkout sweep must remain bounded to the recovery window")
	_, err = svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetCheckoutStartedAt(time.Now().Add(-time.Hour)).Save(ctx)
	require.NoError(t, err)
	_, err = svc.reconcileRecentCheckoutOrders(ctx)
	require.NoError(t, err)
	current, err = svc.entClient.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, current.Status)
}

func TestCheckoutMethodsAcceptHostedModesAndRejectStaleQuote(t *testing.T) {
	ctx := context.Background()
	settings := map[string]string{}
	svc, user := newCheckoutOrderTestService(t, settings)
	addCheckoutEasyPay(t, svc, "http://127.0.0.1:1", "popup", "")
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "quote"})
	require.NoError(t, err)
	methods, err := svc.GetCheckoutMethods(ctx, order)
	require.NoError(t, err)
	require.Contains(t, methods, payment.TypeAlipay)
	quote, err := svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	settings[SettingRechargeFeeRate] = "5"
	_, err = svc.PayCheckoutOrder(ctx, order, CheckoutPayRequest{PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test"})
	require.Equal(t, "CHECKOUT_QUOTE_CHANGED", infraerrors.Reason(err))
	quote, err = svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	result, err := svc.PayCheckoutOrder(ctx, order, CheckoutPayRequest{PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test"})
	require.NoError(t, err)
	hosted, err := url.Parse(result.PayURL)
	require.NoError(t, err)
	require.Equal(t, "/submit.php", hosted.Path)
	require.Equal(t, order.OutTradeNo, hosted.Query().Get("out_trade_no"))
	require.Equal(t, "https://app.example.test/payment/checkout/"+order.OutTradeNo, hosted.Query().Get("return_url"))
	current, err := svc.GetPublicCheckoutOrder(ctx, order.OutTradeNo)
	require.NoError(t, err)
	require.False(t, svc.CanSwitchCheckoutMethod(current, svc.IssueCheckoutActionToken(order)))
	_, err = CanonicalizeReturnURL("https://app.example.test/payment/checkout/result", "app.example.test", "")
	require.Equal(t, "INVALID_RETURN_URL", infraerrors.Reason(err))
}

func TestCheckoutSDKMethodsUseConfiguredCurrencyAndPersistAction(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{})
	_, err := svc.entClient.PaymentProviderInstance.Create().SetProviderKey(payment.TypeStripe).SetSupportedTypes("card,alipay,wxpay").
		SetConfig(`{"secretKey":"sk_test_fake","publishableKey":"pk_test_fake","currency":"KWD"}`).Save(ctx)
	require.NoError(t, err)
	_, err = svc.entClient.PaymentProviderInstance.Create().SetProviderKey(payment.TypeAirwallex).SetSupportedTypes("airwallex").
		SetConfig(`{"clientId":"client","apiKey":"secret","webhookSecret":"webhook","apiBase":"https://api-demo.airwallex.com/api/v1","currency":"USD"}`).Save(ctx)
	require.NoError(t, err)
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{Amount: 20, IdempotencyKey: "sdk"})
	require.NoError(t, err)
	methods, err := svc.GetCheckoutMethods(ctx, order)
	require.NoError(t, err)
	require.Len(t, methods, 2)
	require.Equal(t, "KWD", methods[payment.TypeStripe].Currency)
	require.Equal(t, "USD", methods[payment.TypeAirwallex].Currency)
	quote, err := svc.QuoteCheckoutOrder(ctx, order, payment.TypeStripe)
	require.NoError(t, err)
	require.Equal(t, "KWD", quote.Currency)
	started, err := svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusPending).
		SetCheckoutStartedAt(time.Now()).SetPaymentType(payment.TypeStripe).SetProviderKey(payment.TypeStripe).
		SetProviderSnapshot(map[string]any{"currency": "KWD"}).Save(ctx)
	require.NoError(t, err)
	require.Empty(t, paymentOrderQueryReference(started, nil))
	launch := &payment.CreatePaymentResponse{TradeNo: "pi_test", ClientSecret: "pi_test_secret"}
	action := checkoutLaunchAction(&payment.InstanceSelection{ProviderKey: payment.TypeStripe, Config: map[string]string{"publishableKey": "pk_test_fake"}}, launch)
	require.NoError(t, svc.persistCheckoutLaunch(ctx, started, launch, action))
	current, err := svc.GetPublicCheckoutOrder(ctx, order.OutTradeNo)
	require.NoError(t, err)
	require.Equal(t, "pi_test", paymentOrderQueryReference(current, nil))
	replayed, err := svc.PayCheckoutOrder(ctx, current, CheckoutPayRequest{PaymentType: payment.TypeStripe, SiteOrigin: "https://app.example.test"})
	require.NoError(t, err)
	require.Equal(t, action, replayed.Action)
	require.False(t, svc.CanSwitchCheckoutMethod(current, svc.IssueCheckoutActionToken(order)))
	current.Status = OrderStatusCompleted
	require.Nil(t, GetCheckoutPaymentAction(current))
}

type checkoutGroupRepository struct{ GroupRepository }

func (checkoutGroupRepository) GetByID(context.Context, int64) (*Group, error) {
	return &Group{Status: payment.EntityStatusActive, SubscriptionType: SubscriptionTypeSubscription}, nil
}

func TestCheckoutSubscriptionSnapshotsPlanAndRejectsChanges(t *testing.T) {
	ctx := context.Background()
	svc, user := newCheckoutOrderTestService(t, map[string]string{SettingSubscriptionUSDToCNYRate: "7"})
	svc.groupRepo = checkoutGroupRepository{}
	addCheckoutEasyPay(t, svc, "http://127.0.0.1:1", "", "")
	plan, err := svc.entClient.SubscriptionPlan.Create().SetGroupID(123).SetName("Monthly").SetPrice(20).
		SetCurrency("USD").SetValidityDays(1).SetValidityUnit("month").Save(ctx)
	require.NoError(t, err)
	order, err := svc.CreateCheckoutOrder(ctx, user.ID, CheckoutCreateRequest{
		OrderType: payment.OrderTypeSubscription, PlanID: plan.ID, IdempotencyKey: "plan-test",
	})
	require.NoError(t, err)
	require.Equal(t, "Monthly", psStringValue(order.CheckoutPlanName))
	require.Equal(t, "USD", psStringValue(order.CheckoutPlanCurrency))
	quote, err := svc.QuoteCheckoutOrder(ctx, order, "alipay")
	require.NoError(t, err)
	require.Equal(t, 140.0, quote.PayAmount)
	require.Equal(t, "CNY", quote.Currency)
	_, err = svc.entClient.SubscriptionPlan.UpdateOneID(plan.ID).SetCurrency("CNY").Save(ctx)
	require.NoError(t, err)
	_, err = svc.PayCheckoutOrder(ctx, order, CheckoutPayRequest{
		PaymentType: "alipay", QuoteID: quote.QuoteID, SiteOrigin: "https://app.example.test",
	})
	require.Equal(t, "CHECKOUT_PLAN_CHANGED", infraerrors.Reason(err))
}
