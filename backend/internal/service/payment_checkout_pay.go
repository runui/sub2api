package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/ent/subscriptionplan"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/shopspring/decimal"
)

const checkoutProviderTimeout = 20 * time.Second

type CheckoutPayRequest struct {
	PaymentType       string
	QuoteID           string
	SiteOrigin        string // Trusted site origin from server configuration, not browser input.
	ClientIP          string
	IsMobile          bool
	IsWeChatBrowser   bool
	WeChatResumeToken string
}

type CheckoutQuote struct {
	PaymentType string  `json:"payment_type"`
	PayAmount   float64 `json:"pay_amount"`
	Currency    string  `json:"currency"`
	Amount      float64 `json:"amount"`
	FeeRate     float64 `json:"fee_rate"`
	QuoteID     string  `json:"quote_id"`
}

type checkoutPricing struct {
	CheckoutQuote
	limitAmount, bonusAmount float64
	payAmountText            string
	plan                     *dbent.SubscriptionPlan
}

type CheckoutPaymentAction struct {
	ClientSecret   string                      `json:"client_secret,omitempty"`
	PublishableKey string                      `json:"publishable_key,omitempty"`
	IntentID       string                      `json:"intent_id,omitempty"`
	CountryCode    string                      `json:"country_code,omitempty"`
	PaymentEnv     string                      `json:"payment_env,omitempty"`
	JSAPI          *payment.WechatJSAPIPayload `json:"jsapi,omitempty"`
	AlipayDeepLink bool                        `json:"alipay_deep_link,omitempty"`
}

type CheckoutPayResponse struct {
	*CreateOrderResponse
	Action *CheckoutPaymentAction
}

func (s *PaymentService) GetCheckoutMethods(ctx context.Context, order *dbent.PaymentOrder) (map[string]MethodLimits, error) {
	methods := make(map[string]MethodLimits)
	if order == nil || order.CheckoutKeyHash == nil || order.Status != OrderStatusAwaitingConfirmation || !order.ExpiresAt.After(time.Now()) {
		return methods, nil
	}
	cfg, err := s.getCheckoutConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s.checkoutMethods(ctx, cfg)
}

func checkoutSupportsProvider(method, providerKey string) bool {
	switch method {
	case payment.TypeAlipay, payment.TypeWxpay:
		return providerKey == method || providerKey == payment.TypeEasyPay
	case payment.TypeStripe, payment.TypeAirwallex:
		return providerKey == method
	default:
		return method != "" && providerKey == payment.TypeEasyPay
	}
}

func checkoutInstanceSupportsMethod(inst *dbent.PaymentProviderInstance, method string) bool {
	if !inst.Enabled || !checkoutSupportsProvider(method, inst.ProviderKey) {
		return false
	}
	switch method {
	case payment.TypeStripe:
		return true // supported_types contains Stripe's sub-methods.
	case payment.TypeAirwallex:
		return payment.InstanceSupportsType(inst.SupportedTypes, method)
	default:
		return providerSupportsVisibleMethod(inst, method)
	}
}

func (s *PaymentService) checkoutMethods(ctx context.Context, cfg *PaymentConfig) (map[string]MethodLimits, error) {
	instances, err := s.entClient.PaymentProviderInstance.Query().Where(paymentproviderinstance.EnabledEQ(true)).All(ctx)
	if err != nil {
		return nil, err
	}
	methods := make(map[string]MethodLimits)
	methodSet := map[string]struct{}{payment.TypeAlipay: {}, payment.TypeWxpay: {}, payment.TypeStripe: {}, payment.TypeAirwallex: {}}
	for _, instance := range instances {
		if instance.ProviderKey == payment.TypeEasyPay {
			for _, method := range enabledVisibleMethodsForProvider(instance.ProviderKey, instance.SupportedTypes) {
				methodSet[method] = struct{}{}
			}
		}
	}
	for method := range methodSet {
		var matching []*dbent.PaymentProviderInstance
		for _, inst := range instances {
			if checkoutInstanceSupportsMethod(inst, method) {
				matching = append(matching, inst)
			}
		}
		if key := visibleMethodEnabledSettingKey(method); key != "" {
			enabled, err := s.configService.settingRepo.GetValue(ctx, key)
			if err != nil && !errors.Is(err, ErrSettingNotFound) {
				return nil, err
			}
			if enabled == "false" {
				continue
			}
			key, err := s.configService.resolveVisibleMethodProviderKey(ctx, method, matching)
			if err != nil {
				if infraerrors.Reason(err) == "INVALID_PAYMENT_VISIBLE_METHOD_SOURCE" {
					continue
				}
				return nil, err
			}
			if key != "" {
				matching = filterVisibleMethodInstancesByProviderKey(matching, method, key)
			}
		}
		// The legacy load balancer can choose any routed instance. Advertise
		// only routes where every candidate can be launched and reconciled.
		launchable := len(matching) > 0
		for _, instance := range matching {
			config, err := s.configService.decryptConfig(instance.Config)
			if err != nil || (method == payment.TypeStripe && strings.TrimSpace(config[payment.ConfigKeyPublishableKey]) == "") {
				launchable = false
				break
			}
			if _, err := provider.CreateProvider(instance.ProviderKey, strconv.FormatInt(instance.ID, 10), config); err != nil {
				launchable = false
				break
			}
		}
		if !launchable {
			continue
		}
		currency, ok := s.configService.pcAggregateMethodCurrency(matching)
		if !ok {
			continue
		}
		limits := pcAggregateMethodLimits(method, matching)
		limits.Currency = currency
		limits.FeeRate = cfg.RechargeFeeRate
		limits.DisplayName = s.configService.pcAggregateMethodDisplayName(method, matching)
		methods[method] = limits
	}
	return methods, nil
}

func (s *PaymentService) QuoteCheckoutOrder(ctx context.Context, order *dbent.PaymentOrder, paymentType string) (*CheckoutQuote, error) {
	if order == nil {
		return nil, infraerrors.NotFound("NOT_FOUND", "checkout order not found")
	}
	current, err := s.GetCheckoutOrder(ctx, order.ID, order.UserID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.getCheckoutConfig(ctx)
	if err != nil {
		return nil, err
	}
	quote, err := s.priceCheckout(ctx, current, NormalizeVisibleMethod(paymentType), cfg)
	if err != nil {
		return nil, err
	}
	return &quote.CheckoutQuote, nil
}

func checkoutNotPayable() error {
	return infraerrors.Conflict("CHECKOUT_NOT_PAYABLE", "checkout order is no longer awaiting confirmation")
}

func (s *PaymentService) priceCheckout(ctx context.Context, order *dbent.PaymentOrder, method string, cfg *PaymentConfig) (*checkoutPricing, error) {
	if order.Status != OrderStatusAwaitingConfirmation || !order.ExpiresAt.After(time.Now()) {
		return nil, checkoutNotPayable()
	}
	methods, err := s.checkoutMethods(ctx, cfg)
	if err != nil {
		return nil, err
	}
	limits, ok := methods[method]
	if !ok {
		return nil, infraerrors.BadRequest("CHECKOUT_METHOD_UNAVAILABLE", "payment method is unavailable for checkout")
	}
	req := CreateOrderRequest{OrderType: order.OrderType, Amount: order.Amount}
	if order.PlanID != nil {
		req.PlanID = *order.PlanID
	}
	plan, err := s.validateOrderInput(ctx, req, cfg)
	if err != nil {
		return nil, err
	}
	if plan != nil && !checkoutPlanMatches(order, plan) {
		return nil, infraerrors.Conflict("CHECKOUT_PLAN_CHANGED", "subscription plan has changed")
	}
	price := &checkoutPricing{CheckoutQuote: CheckoutQuote{
		PaymentType: method, Amount: order.Amount, FeeRate: cfg.RechargeFeeRate, Currency: limits.Currency,
	}, limitAmount: order.Amount, plan: plan}
	if !rechargeBonusValueValid(price.FeeRate, 100) {
		return nil, infraerrors.ServiceUnavailable("PAYMENT_CONFIG_INVALID", "payment fee configuration is invalid")
	}
	if order.OrderType == payment.OrderTypeBalance {
		bonus := quoteRechargeBonus(cfg, order.Amount, limits.Currency)
		price.limitAmount, price.Amount, price.bonusAmount = bonus.PayBase, bonus.Credited, bonus.Bonus
	}
	price.payAmountText, price.PayAmount, err = calculateCreateOrderPayAmountForOrderType(
		price.limitAmount, price.FeeRate, price.Currency, order.OrderType, cfg.SubscriptionUSDToCNYRate)
	if err != nil {
		return nil, err
	}
	if !isValidProviderAmount(price.PayAmount) || price.PayAmount > 1e13 || !isValidProviderAmount(price.Amount) || price.Amount > 1e15 {
		return nil, infraerrors.BadRequest("INVALID_AMOUNT", "amount out of range")
	}
	if (limits.SingleMin > 0 && price.PayAmount < limits.SingleMin) || (limits.SingleMax > 0 && price.PayAmount > limits.SingleMax) {
		return nil, checkoutInstanceLimitError()
	}
	resume := s.paymentResume()
	if err := resume.ensureSigningKey(); err != nil {
		return nil, err
	}
	// A quote is stateless and bound to every value that affects the charge or
	// fulfillment. Recomputing at pay rejects stale prices without a new row.
	// Its lifetime is bounded by the unconfirmed order.
	price.QuoteID = resume.sign(fmt.Sprintf("checkout-quote-v1:%d:%d:%s:%s:%s:%.8f:%.8f:%.8f:%.8f",
		order.ID, order.UserID, method, price.Currency, price.payAmountText, price.Amount, price.FeeRate, price.bonusAmount, price.limitAmount))
	return price, nil
}

func checkoutPlanMatches(order *dbent.PaymentOrder, plan *dbent.SubscriptionPlan) bool {
	return plan.ForSale && order.SubscriptionGroupID != nil && *order.SubscriptionGroupID == plan.GroupID &&
		order.Amount == plan.Price && order.SubscriptionDays != nil &&
		*order.SubscriptionDays == psComputeValidityDays(plan.ValidityDays, plan.ValidityUnit) &&
		psStringValue(order.CheckoutPlanName) == plan.Name && psStringValue(order.CheckoutPlanCurrency) == plan.Currency
}

func (s *PaymentService) CheckCheckoutQuoteSigning() error {
	return s.paymentResume().ensureSigningKey()
}

func checkoutResultURL(siteOrigin, orderNumber string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(siteOrigin))
	if err != nil || u.User != nil || u.Opaque != "" || !isPlainReturnURLHost(u.Hostname()) ||
		u.Scheme != "https" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", infraerrors.BadRequest("INVALID_CHECKOUT_ORIGIN", "checkout requires a trusted HTTPS site origin")
	}
	// Never accept a checkout result path via the legacy client return_url.
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/payment/checkout/" + orderNumber}).String(), nil
}

func (s *PaymentService) PayCheckoutOrder(ctx context.Context, order *dbent.PaymentOrder, req CheckoutPayRequest) (*CheckoutPayResponse, error) {
	if order == nil {
		return nil, infraerrors.NotFound("NOT_FOUND", "checkout order not found")
	}
	current, err := s.GetCheckoutOrder(ctx, order.ID, order.UserID)
	if err != nil {
		return nil, err
	}
	method := NormalizeVisibleMethod(req.PaymentType)
	returnURL, err := checkoutResultURL(req.SiteOrigin, current.OutTradeNo)
	if err != nil {
		return nil, err
	}
	if current.CheckoutStartedAt != nil {
		return s.checkoutPayReplay(current, method)
	}
	cfg, err := s.getCheckoutConfig(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkCancelRateLimit(ctx, current.UserID, cfg); err != nil {
		return nil, err
	}
	price, err := s.priceCheckout(ctx, current, method, cfg)
	if err != nil {
		return nil, err
	}
	if len(req.QuoteID) > 128 || price.QuoteID != req.QuoteID {
		return nil, infraerrors.Conflict("CHECKOUT_QUOTE_CHANGED", "refresh the checkout quote before paying")
	}
	legacyReq := CreateOrderRequest{UserID: current.UserID, Amount: current.Amount, OrderType: current.OrderType,
		PaymentType: method, IsMobile: req.IsMobile, IsWeChatBrowser: req.IsWeChatBrowser, SrcURL: returnURL}
	if method == payment.TypeWxpay && req.IsWeChatBrowser && req.WeChatResumeToken == "" && s.usesOfficialWxpayVisibleMethod(ctx) {
		if _, _, err := s.getWeChatPaymentOAuthCredential(ctx); err != nil {
			// Native remains available to merchants without the optional MP OAuth setup.
			legacyReq.IsWeChatBrowser, legacyReq.IsMobile = false, false
		}
	}
	if method == payment.TypeWxpay && req.IsWeChatBrowser && req.WeChatResumeToken != "" {
		claims, err := s.ParseWeChatPaymentResumeToken(req.WeChatResumeToken)
		if err != nil {
			return nil, err
		}
		if claims.PaymentType != method || claims.RedirectTo != "/payment/checkout/"+current.OutTradeNo {
			return nil, infraerrors.BadRequest("INVALID_WECHAT_PAYMENT_RESUME_TOKEN", "wechat payment resume token does not match checkout")
		}
		legacyReq.OpenID = claims.OpenID
	}
	if ip := net.ParseIP(strings.TrimSpace(req.ClientIP)); ip != nil {
		legacyReq.ClientIP = ip.String()
	}
	var sel *payment.InstanceSelection
	switch method {
	case payment.TypeAlipay, payment.TypeWxpay, payment.TypeStripe:
		sel, err = s.selectCreateOrderInstance(ctx, legacyReq, cfg, price.PayAmount)
	default:
		key := payment.TypeEasyPay
		if method == payment.TypeAirwallex {
			key = payment.TypeAirwallex
		}
		sel, err = s.loadBalancer.SelectInstance(ctx, key, method, payment.Strategy(cfg.LoadBalanceStrategy), price.PayAmount)
		if err != nil {
			return nil, checkoutInstanceLimitError()
		}
	}
	if err != nil {
		return nil, err
	}
	if sel == nil || !checkoutSupportsProvider(method, sel.ProviderKey) ||
		price.Currency != paymentProviderConfigCurrency(sel.ProviderKey, sel.Config) {
		return nil, infraerrors.Conflict("CHECKOUT_METHOD_CHANGED", "payment provider changed; refresh checkout methods")
	}
	if err := s.validateSelectedCreateOrderInstance(ctx, legacyReq, sel); err != nil {
		return nil, err
	}
	if oauth, err := s.maybeBuildWeChatOAuthRequiredResponseForSelection(ctx, legacyReq, price.Amount, price.PayAmount, price.FeeRate, sel); oauth != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return &CheckoutPayResponse{CreateOrderResponse: oauth}, nil
	}
	if sel.ProviderKey == payment.TypeWxpay && legacyReq.IsMobile && legacyReq.OpenID == "" && legacyReq.ClientIP == "" {
		return nil, infraerrors.BadRequest("CHECKOUT_CLIENT_IP_REQUIRED", "wechat H5 payment requires client IP")
	}
	if sel.ProviderKey == payment.TypeStripe && strings.TrimSpace(sel.Config[payment.ConfigKeyPublishableKey]) == "" {
		return nil, infraerrors.ServiceUnavailable("PAYMENT_PROVIDER_MISCONFIGURED", "provider_misconfigured")
	}
	prov, err := provider.CreateProvider(sel.ProviderKey, sel.InstanceID, sel.Config)
	if err != nil {
		return nil, infraerrors.ServiceUnavailable("PAYMENT_PROVIDER_MISCONFIGURED", "provider_misconfigured")
	}
	return s.reserveAndLaunchCheckout(ctx, current, legacyReq, cfg, price, sel, prov, returnURL)
}

func checkoutInstanceLimitError() error {
	return infraerrors.TooManyRequests("NO_AVAILABLE_INSTANCE", "payment method amount or capacity limit reached")
}

func (s *PaymentService) reserveAndLaunchCheckout(ctx context.Context, order *dbent.PaymentOrder, req CreateOrderRequest,
	cfg *PaymentConfig, price *checkoutPricing, sel *payment.InstanceSelection, prov payment.Provider, returnURL string) (*CheckoutPayResponse, error) {
	reserved, won, err := s.reserveCheckout(ctx, order, req, cfg, price, sel, returnURL)
	if err != nil {
		return nil, err
	}
	if !won {
		return s.checkoutPayReplay(reserved, req.PaymentType)
	}
	providerReq := buildProviderCreatePaymentRequest(CreateOrderRequest{
		PaymentType: req.PaymentType, ClientIP: req.ClientIP, IsMobile: req.IsMobile, OpenID: req.OpenID, ReturnURL: returnURL,
	}, sel, reserved.OutTradeNo, price.payAmountText, s.buildPaymentSubject(price.plan, price.limitAmount, cfg, sel))
	providerReq.WxpayNativeOnH5NotAuthorized = sel.ProviderKey == payment.TypeWxpay
	providerReq.ExpiresAt = &reserved.ExpiresAt
	if sel.ProviderKey == payment.TypeAlipay {
		mode := strings.TrimSpace(sel.Config["paymentMode"])
		providerReq.AlipayMobilePrecreate = !strings.EqualFold(mode, "redirect") &&
			(!req.IsMobile || cfg.AlipayForceQRCode || cfg.AlipayMobilePrecreateDeepLink)
		// For QR issue precreate only; an uncertain call must not fall back to
		// a different channel operation under the same merchant trade number.
		providerReq.IsMobile = providerReq.AlipayMobilePrecreate || (req.IsMobile && !cfg.AlipayForceQRCode)
	} else if req.PaymentType == payment.TypeAlipay && cfg.AlipayForceQRCode && !cfg.AlipayMobilePrecreateDeepLink {
		providerReq.IsMobile = false
	}

	// The reservation commits before contacting the provider. Detach from the
	// browser request so a closed tab cannot interrupt the response write. An
	// uncertain create never clears the reservation; callbacks/query own recovery.
	work := context.WithoutCancel(ctx)
	callCtx, cancel := context.WithTimeout(work, checkoutProviderTimeout)
	finish := servertiming.ObserveDependency(callCtx, "payment")
	pr, callErr := prov.CreatePayment(callCtx, providerReq)
	finish()
	cancel()

	writeCtx, cancel := context.WithTimeout(work, 5*time.Second)
	defer cancel()
	if callErr != nil || pr == nil {
		// Provider error text can contain request details; do not return or log it.
		slog.Warn("checkout provider create outcome uncertain", "orderID", reserved.ID, "provider", sel.ProviderKey)
	} else {
		action := checkoutLaunchAction(sel, pr)
		if req.IsMobile && cfg.AlipayMobilePrecreateDeepLink && providerReq.AlipayMobilePrecreate && pr.QRCode != "" {
			action = &CheckoutPaymentAction{AlipayDeepLink: true}
		}
		if err := s.persistCheckoutLaunch(writeCtx, reserved, pr, action); err != nil {
			return nil, err
		}
	}
	s.writeAuditLog(writeCtx, reserved.ID, "ORDER_CREATED", fmt.Sprintf("user:%d", reserved.UserID), map[string]any{
		"paymentAmount": req.Amount, "creditedAmount": reserved.Amount, "bonusAmount": reserved.BonusAmount,
		"payAmount": reserved.PayAmount, "paymentType": reserved.PaymentType, "orderType": reserved.OrderType,
		"paymentSource": "existing_checkout",
	})
	current, err := s.entClient.PaymentOrder.Get(writeCtx, reserved.ID)
	if err != nil {
		return nil, err
	}
	return buildCheckoutPayResponse(current), nil
}

func (s *PaymentService) reserveCheckout(ctx context.Context, order *dbent.PaymentOrder, req CreateOrderRequest,
	cfg *PaymentConfig, price *checkoutPricing, sel *payment.InstanceSelection, returnURL string) (*dbent.PaymentOrder, bool, error) {
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Hold the user row while counting and reserving so two checkout orders from
	// this user cannot both take the last pending slot. Never hold locks across I/O.
	if _, err := lockCheckoutUser(ctx, tx, order.UserID); err != nil {
		return nil, false, err
	}
	current, err := tx.PaymentOrder.Get(ctx, order.ID)
	if err != nil {
		return nil, false, err
	}
	if current.CheckoutStartedAt != nil {
		return current, false, nil
	}
	if current.Status != OrderStatusAwaitingConfirmation || !current.ExpiresAt.After(time.Now()) {
		return nil, false, checkoutNotPayable()
	}
	if current.PlanID != nil {
		plan, err := tx.SubscriptionPlan.Query().Unique(false).Where(subscriptionplan.IDEQ(*current.PlanID), checkoutForUpdate).Only(ctx)
		if dbent.IsNotFound(err) || (err == nil && (price.plan == nil || !checkoutPlanMatches(current, plan) || price.plan.ProductName != plan.ProductName)) {
			return nil, false, infraerrors.Conflict("CHECKOUT_PLAN_CHANGED", "subscription plan has changed")
		}
		if err != nil {
			return nil, false, err
		}
	}
	if err := s.checkPendingLimit(ctx, tx, order.UserID, cfg.MaxPendingOrders); err != nil {
		return nil, false, err
	}
	if err := s.checkDailyLimit(ctx, tx, order.UserID, price.limitAmount, cfg.DailyLimit); err != nil {
		return nil, false, err
	}
	if err := s.checkCheckoutInstanceCapacity(ctx, tx, sel, req.PaymentType, price.PayAmount); err != nil {
		return nil, false, err
	}
	now := time.Now()
	tm := cfg.OrderTimeoutMin
	if tm <= 0 {
		tm = defaultOrderTimeoutMin
	}
	// The conditional transition is the one and only permission to call create.
	// Commit pricing and provider identity against the original merchant number,
	// before a webhook or the reconciler can observe the PENDING status.
	snapshot := buildPaymentOrderProviderSnapshot(sel, req)
	// The settled Amount includes multipliers and bonuses; it cannot recover the original input.
	snapshot["checkout_input_amount"] = current.Amount
	reserved, err := tx.PaymentOrder.UpdateOneID(order.ID).Where(
		paymentorder.UserIDEQ(order.UserID), paymentorder.CheckoutKeyHashNotNil(), paymentorder.CheckoutStartedAtIsNil(),
		paymentorder.StatusEQ(OrderStatusAwaitingConfirmation), paymentorder.ExpiresAtGT(now),
	).SetStatus(OrderStatusPending).SetCheckoutStartedAt(now).SetExpiresAt(now.Add(time.Duration(tm) * time.Minute)).
		SetPaymentType(req.PaymentType).SetRechargeCode(fmt.Sprintf("PAY-%d-%d", order.ID, now.UnixNano()%100000)).
		SetAmount(price.Amount).SetBonusAmount(price.bonusAmount).SetFeeRate(price.FeeRate).SetPayAmount(price.PayAmount).
		SetProviderInstanceID(sel.InstanceID).SetProviderKey(sel.ProviderKey).SetProviderSnapshot(snapshot).
		SetClientIP(req.ClientIP).SetSrcURL(returnURL).Save(ctx)
	if dbent.IsNotFound(err) {
		return nil, false, checkoutNotPayable()
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return reserved, true, nil
}

func (s *PaymentService) checkCheckoutInstanceCapacity(ctx context.Context, tx *dbent.Tx, sel *payment.InstanceSelection, method string, amount float64) error {
	id, err := strconv.ParseInt(sel.InstanceID, 10, 64)
	if err != nil || id <= 0 {
		return checkoutInstanceLimitError()
	}
	// Legacy routing falls back when all instances exceed capacity. For an
	// explicit checkout, enforce the selected instance's limits before reserving.
	instance, err := tx.PaymentProviderInstance.Query().Unique(false).Where(paymentproviderinstance.IDEQ(id), checkoutForUpdate).Only(ctx)
	if dbent.IsNotFound(err) || (err == nil && (instance.ProviderKey != sel.ProviderKey || !checkoutInstanceSupportsMethod(instance, method))) {
		return checkoutInstanceLimitError()
	}
	if err != nil {
		return err
	}
	config, err := s.configService.decryptConfig(instance.Config)
	if err != nil {
		return err
	}
	if instance.PaymentMode != "" {
		if config == nil {
			config = make(map[string]string)
		}
		config["paymentMode"] = instance.PaymentMode
	}
	// Do not reserve with stale credentials if the instance changed between
	// routing and the transaction. Existing callbacks still use the pinned instance.
	if sel.PaymentMode != instance.PaymentMode || sel.SupportedTypes != instance.SupportedTypes || !maps.Equal(sel.Config, config) {
		return infraerrors.Conflict("CHECKOUT_METHOD_CHANGED", "payment provider changed; refresh checkout methods")
	}
	limits, ok := pcInstanceTypeLimits(instance, method)
	if !ok {
		limits, _ = pcInstanceTypeLimits(instance, method+"_direct")
	}
	if (limits.SingleMin > 0 && amount < limits.SingleMin) || (limits.SingleMax > 0 && amount > limits.SingleMax) {
		return checkoutInstanceLimitError()
	}
	if limits.DailyLimit > 0 {
		now := time.Now()
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		orders, err := tx.PaymentOrder.Query().Where(paymentorder.ProviderInstanceIDEQ(sel.InstanceID),
			paymentorder.StatusIn(OrderStatusPending, OrderStatusPaid, OrderStatusRecharging, OrderStatusCompleted),
			paymentorder.Or(paymentorder.CheckoutStartedAtGTE(start),
				paymentorder.And(paymentorder.CheckoutStartedAtIsNil(), paymentorder.CreatedAtGTE(start)))).
			Select(paymentorder.FieldPayAmount).All(ctx)
		if err != nil {
			return err
		}
		used := decimal.NewFromFloat(amount)
		for _, order := range orders {
			used = used.Add(decimal.NewFromFloat(order.PayAmount))
		}
		if used.GreaterThan(decimal.NewFromFloat(limits.DailyLimit)) {
			return checkoutInstanceLimitError()
		}
	}
	return nil
}

func checkoutLaunchAction(sel *payment.InstanceSelection, pr *payment.CreatePaymentResponse) *CheckoutPaymentAction {
	switch sel.ProviderKey {
	case payment.TypeStripe:
		if pr.ClientSecret != "" {
			return &CheckoutPaymentAction{ClientSecret: pr.ClientSecret, PublishableKey: sel.Config[payment.ConfigKeyPublishableKey]}
		}
	case payment.TypeAirwallex:
		if pr.ClientSecret != "" && pr.IntentID != "" {
			return &CheckoutPaymentAction{ClientSecret: pr.ClientSecret, IntentID: pr.IntentID, CountryCode: pr.CountryCode, PaymentEnv: pr.PaymentEnv}
		}
	case payment.TypeWxpay:
		if pr.JSAPI != nil {
			return &CheckoutPaymentAction{JSAPI: pr.JSAPI}
		}
	}
	return nil
}

func GetCheckoutPaymentAction(order *dbent.PaymentOrder) *CheckoutPaymentAction {
	if order.CheckoutStartedAt == nil || order.Status != OrderStatusPending || !order.ExpiresAt.After(time.Now()) {
		return nil
	}
	value, ok := order.ProviderSnapshot["checkout_payment_action"]
	if !ok {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var action CheckoutPaymentAction
	if json.Unmarshal(data, &action) != nil {
		return nil
	}
	return &action
}

func (s *PaymentService) persistCheckoutLaunch(ctx context.Context, order *dbent.PaymentOrder, pr *payment.CreatePaymentResponse, action *CheckoutPaymentAction) error {
	sanitizeCreatePaymentResponseDetails(pr)
	// A callback/reconcile may already have paid or completed the order before
	// create returns. Never change status or overwrite its upstream trade number.
	b := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(order.ID), paymentorder.OrderTypeEQ(order.OrderType),
		paymentorder.StatusEQ(OrderStatusPending), paymentorder.ExpiresAtGT(time.Now()), paymentorder.CheckoutStartedAtNotNil(),
		paymentorder.OutTradeNoEQ(order.OutTradeNo), paymentorder.PaymentTradeNoEQ(""))
	if action != nil {
		// Browser-scoped SDK parameters, not merchant credentials. Persist beside
		// the upstream ID so reloads never have to create another payment.
		snapshot := maps.Clone(order.ProviderSnapshot)
		snapshot["checkout_payment_action"] = action
		b.SetProviderSnapshot(snapshot)
	}
	_, err := b.SetNillablePaymentTradeNo(psNilIfEmpty(pr.TradeNo)).SetNillablePayURL(psNilIfEmpty(pr.PayURL)).
		SetNillableQrCode(psNilIfEmpty(pr.QRCode)).Save(ctx)
	return err
}

func (s *PaymentService) checkoutPayReplay(order *dbent.PaymentOrder, method string) (*CheckoutPayResponse, error) {
	if order.CheckoutStartedAt == nil || order.PaymentType != method {
		return nil, checkoutNotPayable()
	}
	return buildCheckoutPayResponse(order), nil
}

func buildCheckoutPayResponse(order *dbent.PaymentOrder) *CheckoutPayResponse {
	resp := &CreateOrderResponse{
		OrderID: order.ID, Amount: order.Amount, PayAmount: order.PayAmount, BonusAmount: order.BonusAmount,
		FeeRate: order.FeeRate, Currency: PaymentOrderCurrency(order), Status: order.Status, PaymentType: order.PaymentType,
		OutTradeNo: order.OutTradeNo, ExpiresAt: order.ExpiresAt,
		ResultType: payment.CreatePaymentResultOrderCreated,
	}
	if snapshot := psOrderProviderSnapshot(order); snapshot != nil {
		resp.PaymentMode = snapshot.PaymentMode
	}
	if order.Status == OrderStatusPending && order.ExpiresAt.After(time.Now()) {
		resp.PayURL, resp.QRCode = psStringValue(order.PayURL), psStringValue(order.QrCode)
	}
	return &CheckoutPayResponse{CreateOrderResponse: resp, Action: GetCheckoutPaymentAction(order)}
}
