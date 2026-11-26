package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCheckoutSiteUsesConfiguredURLsOnly(t *testing.T) {
	site, err := parseCheckoutSite("https://pay.example.com/", "myapp://payment/return")
	require.NoError(t, err)
	require.Equal(t, "https://pay.example.com", site.origin)
	require.Equal(t, "myapp://payment/return?order_id="+checkoutTestNumber, site.clientReturnURL(checkoutTestNumber))
	for _, bad := range []string{"", "http://pay.example.com", "https://pay.example.com/path", "https://pay.example.com/?", "https://alice@pay.example.com"} {
		_, err := parseCheckoutSite(bad, "")
		require.Error(t, err, bad)
	}
	for _, bad := range []string{"javascript://pay.example.com", "https://app.example.com/?status=success", "intent://app.example.com", "https://app.example.com/#x"} {
		_, err := parseCheckoutSite("https://pay.example.com", bad)
		require.Error(t, err, bad)
	}
}

const checkoutTestNumber = "0123456789abcdef0123456789abcdef"

func TestCheckoutLinkContainsOnlyOrderNumber(t *testing.T) {
	site, err := parseCheckoutSite("https://pay.example.com", "")
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sendCheckoutLink(c, site, &dbent.PaymentOrder{OutTradeNo: checkoutTestNumber}, "action-token")
	require.Contains(t, rec.Body.String(), `"checkout_url":"https://pay.example.com/payment/checkout/`+checkoutTestNumber+`"`)
	require.Contains(t, rec.Body.String(), `"open_url":"https://pay.example.com/payment/checkout/`+checkoutTestNumber+`#action_token=action-token"`)
}

func TestCheckoutProjectionHidesPrivateAndUnquotedFields(t *testing.T) {
	order := &dbent.PaymentOrder{ID: 1, OutTradeNo: checkoutTestNumber, UserEmail: "private@example.com", UserID: 2, Status: "AWAITING_CONFIRMATION"}
	raw, err := json.Marshal(newCheckoutOrderView(order, checkoutSite{}))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "pay_amount")
	require.NotContains(t, string(raw), "private@example.com")
	require.NotContains(t, string(raw), "user_id")
	now := time.Now()
	order.CheckoutStartedAt = &now
	order.PayAmount = 12.50
	raw, err = json.Marshal(newCheckoutOrderView(order, checkoutSite{}))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"pay_amount":12.5`)
	require.NotContains(t, string(raw), "out_trade_no")
}

func TestCheckoutPublicOrderUsesOnlyRandomOrderNumber(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "checkout-integration-test-signing-key"
	t.Setenv("PAYMENT_RESUME_SIGNING_KEY", key)
	t.Setenv("CHECKOUT_PUBLIC_URL", "https://pay.example.com")
	t.Setenv("CHECKOUT_CLIENT_RETURN_URL", "https://app.example.com/return")
	db, err := sql.Open("sqlite", "file:checkout_result_handler?mode=memory&cache=shared")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	owner, err := client.User.Create().SetEmail("checkout@example.com").SetPasswordHash("hash").SetUsername("checkout").Save(ctx)
	require.NoError(t, err)
	order, err := client.PaymentOrder.Create().SetUserID(owner.ID).SetUserEmail(owner.Email).SetUserName(owner.Username).
		SetAmount(10).SetPayAmount(11).SetRechargeCode("PAY-TEST").SetOutTradeNo(checkoutTestNumber).
		SetPaymentType(payment.TypeAlipay).SetPaymentTradeNo("provider-order").SetOrderType(payment.OrderTypeBalance).
		SetStatus(service.OrderStatusPaid).SetExpiresAt(time.Now().Add(time.Hour)).SetPaidAt(time.Now()).
		SetClientIP("127.0.0.1").SetSrcHost("pay.example.com").SetCheckoutKeyHash("checkout-test").
		SetCheckoutStartedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	cfg := service.NewPaymentConfigService(client, nil, []byte(key))
	h := NewPaymentHandler(service.NewPaymentService(client, payment.NewRegistry(), nil, nil, nil, cfg, nil, nil, nil), cfg)
	router := gin.New()
	router.GET("/orders/:id", h.GetCheckoutOrder)
	for _, status := range []string{service.OrderStatusPending, service.OrderStatusPaid, service.OrderStatusCompleted, service.OrderStatusExpired} {
		_, err = order.Update().SetStatus(status).SetPayURL("https://provider.example/pay").SetQrCode("provider-qr").Save(ctx)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+checkoutTestNumber+"?status=success", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"status":"`+status+`"`)
		require.Contains(t, rec.Body.String(), `"order_id":"`+checkoutTestNumber+`"`)
		require.NotContains(t, rec.Body.String(), owner.Email)
		if status == service.OrderStatusPending {
			require.Contains(t, rec.Body.String(), `"pay_url":"https://provider.example/pay"`)
			require.Contains(t, rec.Body.String(), `"qr_code":"provider-qr"`)
		} else {
			require.NotContains(t, rec.Body.String(), `"pay_url"`)
			require.NotContains(t, rec.Body.String(), `"qr_code"`)
		}
		if status == service.OrderStatusCompleted {
			require.Contains(t, rec.Body.String(), `"client_return_url":"https://app.example.com/return?order_id=`+checkoutTestNumber+`"`)
		} else {
			require.NotContains(t, rec.Body.String(), `"client_return_url"`)
		}
	}
	for id, code := range map[string]int{"1": http.StatusBadRequest, "ffffffffffffffffffffffffffffffff": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+id, nil))
		require.Equal(t, code, rec.Code)
	}
}
