package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func checkoutSwitchInput(order *dbent.PaymentOrder) (CheckoutCreateRequest, bool) {
	if order.CheckoutKeyHash == nil || order.CheckoutStartedAt == nil || order.PaidAt != nil {
		return CheckoutCreateRequest{}, false
	}
	switch psStringValue(order.ProviderKey) {
	case payment.TypeAlipay:
		// A locally signed redirect may still create a trade after a failed
		// close attempt. Only the precreated QR flow supports switching.
		if strings.TrimSpace(psStringValue(order.QrCode)) == "" {
			return CheckoutCreateRequest{}, false
		}
	case payment.TypeWxpay:
	default:
		return CheckoutCreateRequest{}, false
	}
	req := CheckoutCreateRequest{OrderType: order.OrderType, IdempotencyKey: "replacement"}
	switch order.OrderType {
	case payment.OrderTypeBalance:
		amount, ok := order.ProviderSnapshot["checkout_input_amount"].(float64)
		if !ok || !isValidProviderAmount(amount) {
			return CheckoutCreateRequest{}, false
		}
		req.Amount = amount
	case payment.OrderTypeSubscription:
		if order.PlanID == nil {
			return CheckoutCreateRequest{}, false
		}
		req.PlanID = *order.PlanID
	default:
		return CheckoutCreateRequest{}, false
	}
	return req, true
}

func checkoutActionPayload(order *dbent.PaymentOrder, expiry string) string {
	return fmt.Sprintf("checkout-action-v1:%s:%d:%s", order.OutTradeNo, order.UserID, expiry)
}

const checkoutActionTTL = time.Hour

// IssueCheckoutActionToken is used by authenticated creation, or after an authorized switch.
func (s *PaymentService) IssueCheckoutActionToken(order *dbent.PaymentOrder) string {
	resume := s.paymentResume()
	if resume.ensureSigningKey() != nil {
		return ""
	}
	expiry := strconv.FormatInt(time.Now().Add(checkoutActionTTL).Unix(), 10)
	return expiry + "." + resume.sign(checkoutActionPayload(order, expiry))
}

func (s *PaymentService) CanSwitchCheckoutMethod(order *dbent.PaymentOrder, token string) bool {
	_, ok := checkoutSwitchInput(order)
	return ok && order.Status == OrderStatusPending && s.validCheckoutActionToken(order, token, time.Now())
}

func (s *PaymentService) validCheckoutActionToken(order *dbent.PaymentOrder, token string, now time.Time) bool {
	if order.CheckoutKeyHash == nil || !now.Before(order.ExpiresAt) || len(token) > 128 {
		return false
	}
	expiry, signature, found := strings.Cut(token, ".")
	deadline, err := strconv.ParseInt(expiry, 10, 64)
	return found && err == nil && now.Unix() < deadline && s.paymentResume().verifySignature(checkoutActionPayload(order, expiry), signature)
}

func (s *PaymentService) CanCancelUnstartedCheckout(order *dbent.PaymentOrder, token string) bool {
	return s.canCancelUnstartedCheckout(order, token, time.Now())
}

func (s *PaymentService) canCancelUnstartedCheckout(order *dbent.PaymentOrder, token string, now time.Time) bool {
	return order.Status == OrderStatusAwaitingConfirmation && order.CheckoutStartedAt == nil && order.PaidAt == nil &&
		s.validCheckoutActionToken(order, token, now)
}

func (s *PaymentService) CancelUnstartedCheckout(ctx context.Context, order *dbent.PaymentOrder, token string) error {
	now := time.Now()
	if !s.canCancelUnstartedCheckout(order, token, now) {
		return infraerrors.Forbidden("CHECKOUT_CANCEL_UNAVAILABLE", "cannot cancel this checkout in this session")
	}
	// Payment reservation and cancellation compete on the same unstarted state.
	n, err := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(order.ID), paymentorder.UserIDEQ(order.UserID),
		paymentorder.StatusEQ(OrderStatusAwaitingConfirmation), paymentorder.CheckoutStartedAtIsNil(),
		paymentorder.PaidAtIsNil(), paymentorder.ExpiresAtGT(now)).SetStatus(OrderStatusCancelled).Save(ctx)
	if err != nil {
		return err
	}
	if n != 1 {
		return infraerrors.Conflict("CHECKOUT_NOT_PAYABLE", "checkout order status has changed")
	}
	s.writeAuditLog(ctx, order.ID, "ORDER_CANCELLED", fmt.Sprintf("user:%d", order.UserID), map[string]any{"detail": "cancel unstarted checkout"})
	return nil
}

func checkoutSwitchUnavailable() error {
	return infraerrors.Forbidden("CHECKOUT_SWITCH_UNAVAILABLE", "cannot change payment method in this session")
}

func (s *PaymentService) SwitchCheckoutMethod(ctx context.Context, order *dbent.PaymentOrder, token string) (*dbent.PaymentOrder, error) {
	const unconfirmed = "CHECKOUT_CANCEL_UNCONFIRMED"
	if !s.CanSwitchCheckoutMethod(order, token) {
		return nil, checkoutSwitchUnavailable()
	}
	// Do not authorize against a stale snapshot supplied by the caller.
	current, err := s.GetCheckoutOrder(ctx, order.ID, order.UserID)
	if err != nil {
		return nil, err
	}
	order = current
	if !s.CanSwitchCheckoutMethod(order, token) {
		return nil, checkoutSwitchUnavailable()
	}
	input, _ := checkoutSwitchInput(order)
	if strings.TrimSpace(psStringValue(order.PayURL)) == "" && strings.TrimSpace(psStringValue(order.QrCode)) == "" && time.Since(*order.CheckoutStartedAt) < checkoutProviderTimeout+5*time.Second {
		return nil, infraerrors.Conflict("CHECKOUT_PAYMENT_STARTING", "payment creation is still in flight")
	}
	cfg, err := s.getCheckoutConfig(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := s.validateOrderInput(ctx, CreateOrderRequest{OrderType: input.OrderType, Amount: input.Amount, PlanID: input.PlanID}, cfg)
	if err != nil {
		return nil, err
	}
	if plan != nil && !checkoutPlanMatches(order, plan) {
		return nil, infraerrors.Conflict("CHECKOUT_PLAN_CHANGED", "subscription plan has changed")
	}
	if result := s.checkPaidWithOptions(ctx, order, checkPaidOptions{cancelIfUnpaid: true, requireClosed: true}); result != checkPaidResultCancelled {
		return nil, infraerrors.Conflict(unconfirmed, "the original payment has not been confirmed closed")
	}
	if !s.CanSwitchCheckoutMethod(order, token) {
		return nil, checkoutSwitchUnavailable()
	}
	return s.createCheckoutOrder(ctx, order.UserID, input, order)
}
