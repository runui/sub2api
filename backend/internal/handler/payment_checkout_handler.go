package handler

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const checkoutPagePath = "/payment/checkout/"

type checkoutSite struct {
	origin       string
	clientReturn *url.URL
}

func parseCheckoutSite(origin, clientReturn string) (checkoutSite, error) {
	site := checkoutSite{}
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return site, infraerrors.ServiceUnavailable("CHECKOUT_NOT_CONFIGURED", "checkout requires a configured HTTPS public origin")
	}
	site.origin = "https://" + u.Host
	if strings.TrimSpace(clientReturn) == "" {
		return site, nil
	}
	u, err = url.Parse(strings.TrimSpace(clientReturn))
	if err != nil || u == nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return site, infraerrors.ServiceUnavailable("CHECKOUT_NOT_CONFIGURED", "invalid checkout client return URL")
	}
	switch u.Scheme {
	case "http", "javascript", "data", "file", "blob", "about", "intent", "market", "chrome", "chrome-extension", "resource", "view-source", "filesystem", "vbscript", "ftp", "ws", "wss":
		return site, infraerrors.ServiceUnavailable("CHECKOUT_NOT_CONFIGURED", "checkout requires an HTTPS or app-specific client return URL")
	}
	site.clientReturn = u
	return site, nil
}

func checkoutSiteFromEnv() (checkoutSite, error) {
	return parseCheckoutSite(os.Getenv("CHECKOUT_PUBLIC_URL"), os.Getenv("CHECKOUT_CLIENT_RETURN_URL"))
}

func (s checkoutSite) clientReturnURL(id string) string {
	if s.clientReturn == nil {
		return ""
	}
	u := *s.clientReturn
	u.RawQuery = url.Values{"order_id": {id}}.Encode()
	return u.String()
}

type checkoutLinkResponse struct {
	OrderID     string    `json:"order_id"`
	CheckoutURL string    `json:"checkout_url"`
	ExpiresAt   time.Time `json:"expires_at"`
	OpenURL     string    `json:"open_url,omitempty"`
}

func sendCheckoutLink(c *gin.Context, site checkoutSite, order *dbent.PaymentOrder, actionToken string) {
	id := checkoutPublicID(order)
	checkoutURL := site.origin + checkoutPagePath + id
	link := checkoutLinkResponse{OrderID: id, CheckoutURL: checkoutURL, ExpiresAt: order.ExpiresAt}
	if actionToken != "" {
		link.OpenURL = checkoutURL + "#" + url.Values{"action_token": {actionToken}}.Encode()
	}
	response.Success(c, link)
}

func (h *PaymentHandler) CreateCheckoutOrder(c *gin.Context) {
	owner, ok := requireAuth(c)
	if !ok {
		return
	}
	if checkoutNewOrdersDisabled() {
		response.ErrorFrom(c, infraerrors.ServiceUnavailable("CHECKOUT_DISABLED", "checkout is not accepting new orders"))
		return
	}
	site, err := checkoutSiteFromEnv()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if err := h.paymentService.CheckCheckoutQuoteSigning(); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	var body struct {
		OrderType string  `json:"order_type" binding:"required"`
		Amount    float64 `json:"amount"`
		PlanID    int64   `json:"plan_id"`
	}
	if err := bindCheckoutJSON(c, &body); err != nil {
		response.BadRequest(c, "invalid checkout order request")
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		response.BadRequest(c, "Idempotency-Key must contain 1 to 128 characters")
		return
	}
	order, err := h.paymentService.CreateCheckoutOrder(c.Request.Context(), owner.UserID, service.CheckoutCreateRequest{
		OrderType: body.OrderType, Amount: body.Amount, PlanID: body.PlanID, IdempotencyKey: key,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	sendCheckoutLink(c, site, order, h.paymentService.IssueCheckoutActionToken(order))
}

func (h *PaymentHandler) ReissueCheckoutLink(c *gin.Context) {
	owner, ok := requireAuth(c)
	if !ok {
		return
	}
	site, err := checkoutSiteFromEnv()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	id, ok := checkoutOrderID(c)
	if !ok {
		return
	}
	order, err := h.paymentService.GetCheckoutOrderByNumber(c.Request.Context(), id, owner.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	sendCheckoutLink(c, site, order, "")
}

func checkoutOrderID(c *gin.Context) (string, bool) {
	raw := c.Param("id")
	if len(raw) != 32 || strings.IndexFunc(raw, func(c rune) bool {
		return (c < '0' || c > '9') && (c < 'a' || c > 'f')
	}) >= 0 {
		response.BadRequest(c, "invalid order ID")
		return "", false
	}
	return raw, true
}

func (h *PaymentHandler) publicCheckoutOrder(c *gin.Context) (*dbent.PaymentOrder, bool) {
	id, ok := checkoutOrderID(c)
	if !ok {
		return nil, false
	}
	order, err := h.paymentService.GetPublicCheckoutOrder(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return nil, false
	}
	return order, true
}

type checkoutOrderView struct {
	CancelReason       string                          `json:"cancel_reason,omitempty"`
	ReplacementOrderID string                          `json:"replacement_order_id,omitempty"`
	OrderID            string                          `json:"order_id"`
	Status             string                          `json:"status"`
	OrderType          string                          `json:"order_type"`
	Amount             float64                         `json:"amount"`
	PayAmount          *float64                        `json:"pay_amount,omitempty"`
	Currency           string                          `json:"currency,omitempty"`
	PlanID             *int64                          `json:"plan_id,omitempty"`
	PlanName           string                          `json:"plan_name,omitempty"`
	PlanCurrency       string                          `json:"plan_currency,omitempty"`
	ExpiresAt          time.Time                       `json:"expires_at"`
	PaymentType        string                          `json:"payment_type,omitempty"`
	ClientReturnURL    string                          `json:"client_return_url,omitempty"`
	Methods            map[string]service.MethodLimits `json:"methods,omitempty"`
	CanPay             *bool                           `json:"can_pay,omitempty"`
	CanSwitchMethod    bool                            `json:"can_switch_method,omitempty"`
	CanCancel          bool                            `json:"can_cancel,omitempty"`
	PayURL             string                          `json:"pay_url,omitempty"`
	QRCode             string                          `json:"qr_code,omitempty"`
	PaymentAction      *service.CheckoutPaymentAction  `json:"payment_action,omitempty"`
}

func newCheckoutOrderView(order *dbent.PaymentOrder, site checkoutSite) checkoutOrderView {
	id := checkoutPublicID(order)
	view := checkoutOrderView{
		OrderID: id, Status: order.Status, OrderType: order.OrderType, Amount: order.Amount,
		PlanID: order.PlanID, ExpiresAt: order.ExpiresAt, PaymentType: order.PaymentType,
	}
	if order.Status == service.OrderStatusCompleted {
		view.ClientReturnURL = site.clientReturnURL(id)
	}
	if order.CheckoutStartedAt != nil {
		view.PayAmount = &order.PayAmount
		view.Currency = service.PaymentOrderCurrency(order)
	}
	if order.CheckoutPlanName != nil {
		view.PlanName = *order.CheckoutPlanName
	}
	if order.CheckoutPlanCurrency != nil {
		view.PlanCurrency = *order.CheckoutPlanCurrency
	}
	return view
}

func checkoutPublicID(order *dbent.PaymentOrder) string {
	return order.OutTradeNo
}

func (h *PaymentHandler) GetOwnedCheckoutOrder(c *gin.Context) {
	owner, ok := requireAuth(c)
	if !ok {
		return
	}
	id, ok := checkoutOrderID(c)
	if !ok {
		return
	}
	order, err := h.paymentService.GetCheckoutOrderByNumber(c.Request.Context(), id, owner.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	view := newCheckoutOrderView(order, checkoutSite{})
	replacementID, err := h.paymentService.OwnedCheckoutReplacementID(c.Request.Context(), order, owner.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if replacementID != "" {
		view.CancelReason = "PAYMENT_METHOD_CHANGED"
		view.ReplacementOrderID = replacementID
	}
	response.Success(c, view)
}

func (h *PaymentHandler) GetCheckoutOrder(c *gin.Context) {
	order, ok := h.publicCheckoutOrder(c)
	if !ok {
		return
	}
	site, _ := checkoutSiteFromEnv()
	view := newCheckoutOrderView(order, site)
	view.CanSwitchMethod = !checkoutNewOrdersDisabled() && h.paymentService.CanSwitchCheckoutMethod(order, c.GetHeader("X-Checkout-Action-Token"))
	view.CanCancel = h.paymentService.CanCancelUnstartedCheckout(order, c.GetHeader("X-Checkout-Action-Token"))
	canPay := time.Now().Before(order.ExpiresAt) &&
		(order.Status == service.OrderStatusAwaitingConfirmation || order.Status == service.OrderStatusPending)
	view.CanPay = &canPay
	if canPay && order.Status == service.OrderStatusPending {
		view.PaymentAction = service.GetCheckoutPaymentAction(order)
		if order.PayURL != nil {
			view.PayURL = *order.PayURL
		}
		if order.QrCode != nil {
			view.QRCode = *order.QrCode
		}
	}
	if canPay && order.Status == service.OrderStatusAwaitingConfirmation {
		methods, err := h.paymentService.GetCheckoutMethods(c.Request.Context(), order)
		if infraerrors.Reason(err) == "PAYMENT_DISABLED" {
			canPay = false
		} else if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		view.Methods = methods
	}
	response.Success(c, view)
}

func (h *PaymentHandler) QuoteCheckoutOrder(c *gin.Context) {
	order, ok := h.publicCheckoutOrder(c)
	if !ok {
		return
	}
	var body struct {
		PaymentType string `json:"payment_type" binding:"required,max=30"`
	}
	if err := bindCheckoutJSON(c, &body); err != nil {
		response.BadRequest(c, "invalid checkout quote request")
		return
	}
	quote, err := h.paymentService.QuoteCheckoutOrder(c.Request.Context(), order, body.PaymentType)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, quote)
}

func (h *PaymentHandler) PayCheckoutOrder(c *gin.Context) {
	order, ok := h.publicCheckoutOrder(c)
	if !ok {
		return
	}
	site, err := checkoutSiteFromEnv()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	var body struct {
		PaymentType       string `json:"payment_type" binding:"required,max=30"`
		QuoteID           string `json:"quote_id" binding:"max=2048"`
		IsMobile          bool   `json:"is_mobile"`
		WeChatResumeToken string `json:"wechat_resume_token" binding:"max=2048"`
	}
	if err := bindCheckoutJSON(c, &body); err != nil {
		response.BadRequest(c, "invalid checkout payment request")
		return
	}
	ipAddr := ip.GetSecurityClientIP(c, false)
	if ipAddr == "" {
		ipAddr = c.ClientIP()
	}
	result, err := h.paymentService.PayCheckoutOrder(c.Request.Context(), order, service.CheckoutPayRequest{
		PaymentType: body.PaymentType, QuoteID: body.QuoteID, SiteOrigin: site.origin, ClientIP: ipAddr, IsMobile: body.IsMobile,
		IsWeChatBrowser: isWeChatBrowser(c), WeChatResumeToken: body.WeChatResumeToken,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// Only project checkout fields; the shared payment response contains internal IDs.
	if result.OAuth != nil {
		response.Success(c, gin.H{"order_id": checkoutPublicID(order), "oauth": result.OAuth})
		return
	}
	view := gin.H{
		"order_id": checkoutPublicID(order), "status": result.Status, "amount": result.Amount,
		"pay_amount": result.PayAmount, "currency": result.Currency, "fee_rate": result.FeeRate,
		"expires_at": result.ExpiresAt, "pay_url": result.PayURL, "qr_code": result.QRCode,
	}
	if result.Action != nil {
		view["payment_action"] = result.Action
	}
	response.Success(c, view)
}

func (h *PaymentHandler) CancelUnstartedCheckout(c *gin.Context) {
	order, ok := h.publicCheckoutOrder(c)
	if !ok {
		return
	}
	if err := h.paymentService.CancelUnstartedCheckout(c.Request.Context(), order, c.GetHeader("X-Checkout-Action-Token")); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": service.OrderStatusCancelled})
}

func (h *PaymentHandler) SwitchCheckoutMethod(c *gin.Context) {
	order, ok := h.publicCheckoutOrder(c)
	if !ok {
		return
	}
	if checkoutNewOrdersDisabled() {
		response.ErrorFrom(c, infraerrors.ServiceUnavailable("CHECKOUT_DISABLED", "checkout is not accepting new orders"))
		return
	}
	site, err := checkoutSiteFromEnv()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// Finish recording a confirmed close even if the browser navigates away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 25*time.Second)
	defer cancel()
	replacement, err := h.paymentService.SwitchCheckoutMethod(ctx, order, c.GetHeader("X-Checkout-Action-Token"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	sendCheckoutLink(c, site, replacement, h.paymentService.IssueCheckoutActionToken(replacement))
}

func bindCheckoutJSON(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	return c.ShouldBindJSON(dst)
}

func checkoutNewOrdersDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CHECKOUT_DISABLE_NEW_ORDERS")), "true")
}
