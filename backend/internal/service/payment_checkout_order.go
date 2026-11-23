package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OrderStatusAwaitingConfirmation = "AWAITING_CONFIRMATION"
	checkoutConfirmationTTL         = 15 * time.Minute
	checkoutCreateWindow            = time.Minute
	checkoutCreateLimit             = 10
	checkoutUnconfirmedLimit        = 20
)

type CheckoutCreateRequest struct {
	Amount         float64
	OrderType      string
	PlanID         int64
	IdempotencyKey string
}

func checkoutHash(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func checkoutReplacementKey(orderNumber string) string {
	// NUL separates server-generated replacement keys from all client keys.
	return checkoutHash("\x00checkout-replacement:" + orderNumber)
}

func normalizeCheckoutCreate(req CheckoutCreateRequest) (CheckoutCreateRequest, string, error) {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if len(req.IdempotencyKey) == 0 || len(req.IdempotencyKey) > 128 {
		return req, "", infraerrors.BadRequest("INVALID_IDEMPOTENCY_KEY", "Idempotency-Key must contain 1 to 128 bytes")
	}
	for _, c := range req.IdempotencyKey {
		if c < '!' || c > '~' || c == ',' {
			return req, "", infraerrors.BadRequest("INVALID_IDEMPOTENCY_KEY", "Idempotency-Key must be a single visible ASCII value")
		}
	}
	if req.OrderType == "" {
		req.OrderType = payment.OrderTypeBalance
	}
	switch req.OrderType {
	case payment.OrderTypeBalance:
		if req.PlanID != 0 || !isValidProviderAmount(req.Amount) || req.Amount > 1e13 {
			return req, "", infraerrors.BadRequest("INVALID_AMOUNT", "balance checkout requires a positive amount and no plan")
		}
		if err := validateCreateOrderAmountCurrency(req.Amount, payment.DefaultPaymentCurrency); err != nil {
			return req, "", err
		}
		return req, checkoutHash("balance:" + strconv.FormatFloat(req.Amount, 'f', 2, 64)), nil
	case payment.OrderTypeSubscription:
		if req.PlanID <= 0 || req.Amount != 0 {
			return req, "", infraerrors.BadRequest("INVALID_INPUT", "subscription checkout requires a plan and no amount")
		}
		return req, checkoutHash("subscription:" + strconv.FormatInt(req.PlanID, 10)), nil
	default:
		return req, "", infraerrors.BadRequest("INVALID_ORDER_TYPE", "unsupported order type")
	}
}

func (s *PaymentService) CreateCheckoutOrder(ctx context.Context, userID int64, input CheckoutCreateRequest) (*dbent.PaymentOrder, error) {
	return s.createCheckoutOrder(ctx, userID, input, nil)
}

func (s *PaymentService) createCheckoutOrder(ctx context.Context, userID int64, input CheckoutCreateRequest, replaces *dbent.PaymentOrder) (*dbent.PaymentOrder, error) {
	req, requestHash, err := normalizeCheckoutCreate(input)
	if err != nil {
		return nil, err
	}
	keyHash := checkoutHash(req.IdempotencyKey)
	if replaces != nil {
		keyHash = checkoutReplacementKey(replaces.OutTradeNo)
	}
	if order, err := findCheckoutReplay(ctx, s.entClient, userID, keyHash, requestHash); order != nil || err != nil {
		if replaces != nil && err == nil {
			return nil, checkoutSwitchUnavailable()
		}
		return order, err
	}
	cfg, err := s.getCheckoutConfig(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := s.validateOrderInput(ctx, CreateOrderRequest{OrderType: req.OrderType, Amount: req.Amount, PlanID: req.PlanID}, cfg)
	if err != nil {
		return nil, err
	}
	if replaces != nil && plan != nil && !checkoutPlanMatches(replaces, plan) {
		return nil, infraerrors.Conflict("CHECKOUT_PLAN_CHANGED", "subscription plan has changed")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	owner, err := lockCheckoutUser(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	// Serialize this user's creations, but replay a matching key even when the
	// new-order limit has been reached. The unique index also enforces this across processes.
	if order, err := findCheckoutReplay(ctx, tx.Client(), userID, keyHash, requestHash); order != nil || err != nil {
		if replaces != nil && err == nil {
			return nil, checkoutSwitchUnavailable()
		}
		return order, err
	}
	now := time.Now()
	if err := checkCheckoutCreateLimits(ctx, tx, userID, now); err != nil {
		return nil, err
	}
	if replaces != nil {
		// Commit the cancellation and replacement together, without overwriting a payment callback.
		n, err := tx.PaymentOrder.Update().Where(paymentorder.IDEQ(replaces.ID), paymentorder.UserIDEQ(userID),
			paymentorder.StatusEQ(OrderStatusPending), paymentorder.PaidAtIsNil(), paymentorder.ExpiresAtGT(now)).
			SetStatus(OrderStatusCancelled).Save(ctx)
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, checkoutSwitchUnavailable()
		}
	}
	// The public checkout ID is the merchant order number, allocated before the
	// link is issued. Reserving it does not create a provider payment.
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return nil, fmt.Errorf("generate checkout order number: %w", err)
	}
	b := tx.PaymentOrder.Create().SetUserID(owner.ID).SetUserEmail(owner.Email).SetUserName(owner.Username).
		SetNillableUserNotes(psNilIfEmpty(owner.Notes)).SetAmount(req.Amount).SetPayAmount(0).
		SetRechargeCode("").SetPaymentType("").SetPaymentTradeNo("").SetClientIP("").SetSrcHost("").
		SetOrderType(req.OrderType).SetStatus(OrderStatusAwaitingConfirmation).SetExpiresAt(now.Add(checkoutConfirmationTTL)).
		SetCheckoutKeyHash(keyHash).SetCheckoutRequestHash(requestHash).SetOutTradeNo(hex.EncodeToString(randomID[:]))
	if plan != nil {
		b.SetAmount(plan.Price).SetPlanID(plan.ID).SetSubscriptionGroupID(plan.GroupID).
			SetCheckoutPlanName(plan.Name).SetCheckoutPlanCurrency(plan.Currency).
			SetSubscriptionDays(psComputeValidityDays(plan.ValidityDays, plan.ValidityUnit))
	}
	order, err := b.Save(ctx)
	if err != nil {
		if replaces == nil && dbent.IsConstraintError(err) {
			_ = tx.Rollback()
			if replay, replayErr := findCheckoutReplay(ctx, s.entClient, userID, keyHash, requestHash); replay != nil || replayErr != nil {
				return replay, replayErr
			}
		}
		return nil, fmt.Errorf("create checkout order: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit checkout order: %w", err)
	}
	if replaces != nil {
		s.writeAuditLog(ctx, replaces.ID, "ORDER_CANCELLED", fmt.Sprintf("user:%d", userID), map[string]any{"detail": "switch checkout payment method"})
	}
	return order, nil
}

func lockCheckoutUser(ctx context.Context, tx *dbent.Tx, userID int64) (*dbent.User, error) {
	owner, err := tx.User.Query().Unique(false).Where(user.IDEQ(userID), checkoutForUpdate).Only(ctx)
	if dbent.IsNotFound(err) || (err == nil && owner.Status != payment.EntityStatusActive) {
		return nil, infraerrors.Forbidden("USER_INACTIVE", "user account is disabled")
	}
	return owner, err
}

func lockPaymentOrderUser(ctx context.Context, tx *dbent.Tx, userID int64) error {
	_, err := tx.User.Query().Unique(false).Where(user.IDEQ(userID), checkoutForUpdate).OnlyID(ctx)
	if dbent.IsNotFound(err) {
		return nil // Leave missing-user validation to the existing legacy path.
	}
	return err
}

func checkoutForUpdate(selector *entsql.Selector) {
	if selector.Dialect() == dialect.Postgres {
		selector.ForUpdate()
	}
}

func findCheckoutReplay(ctx context.Context, client *dbent.Client, userID int64, keyHash, requestHash string) (*dbent.PaymentOrder, error) {
	order, err := client.PaymentOrder.Query().Where(paymentorder.UserIDEQ(userID), paymentorder.CheckoutKeyHashEQ(keyHash)).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if psStringValue(order.CheckoutRequestHash) != requestHash {
		return nil, infraerrors.Conflict("IDEMPOTENCY_KEY_CONFLICT", "Idempotency-Key is already bound to a different checkout request")
	}
	return order, nil
}

func checkCheckoutCreateLimits(ctx context.Context, tx *dbent.Tx, userID int64, now time.Time) error {
	q := tx.PaymentOrder.Query().Where(paymentorder.UserIDEQ(userID), paymentorder.CheckoutKeyHashNotNil())
	recent, err := q.Clone().Where(paymentorder.CreatedAtGTE(now.Add(-checkoutCreateWindow))).Count(ctx)
	if err != nil {
		return err
	}
	active, err := q.Where(paymentorder.StatusEQ(OrderStatusAwaitingConfirmation), paymentorder.ExpiresAtGT(now)).Count(ctx)
	if err != nil {
		return err
	}
	if recent >= checkoutCreateLimit || active >= checkoutUnconfirmedLimit {
		return infraerrors.TooManyRequests("CHECKOUT_CREATE_LIMIT", "too many recent or unconfirmed checkout orders")
	}
	return nil
}

func (s *PaymentService) getCheckoutConfig(ctx context.Context) (*PaymentConfig, error) {
	cfg, err := s.configService.GetPaymentConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, infraerrors.Forbidden("PAYMENT_DISABLED", "payment system is disabled")
	}
	return cfg, nil
}

func (s *PaymentService) GetCheckoutOrder(ctx context.Context, orderID, userID int64) (*dbent.PaymentOrder, error) {
	return s.getCheckoutOrder(ctx, paymentorder.IDEQ(orderID), paymentorder.UserIDEQ(userID))
}

func (s *PaymentService) GetCheckoutOrderByNumber(ctx context.Context, orderNumber string, userID int64) (*dbent.PaymentOrder, error) {
	return s.getCheckoutOrder(ctx, paymentorder.OutTradeNoEQ(orderNumber), paymentorder.UserIDEQ(userID))
}

// OwnedCheckoutReplacementID returns the direct replacement of a cancelled
// checkout without changing the order or status returned by the owner's query.
func (s *PaymentService) OwnedCheckoutReplacementID(ctx context.Context, order *dbent.PaymentOrder, userID int64) (string, error) {
	if order == nil || order.UserID != userID || order.Status != OrderStatusCancelled {
		return "", nil
	}
	replacement, err := s.entClient.PaymentOrder.Query().Where(paymentorder.UserIDEQ(userID),
		paymentorder.CheckoutKeyHashEQ(checkoutReplacementKey(order.OutTradeNo))).Only(ctx)
	if dbent.IsNotFound(err) {
		return "", nil // A direct cancellation has no replacement.
	}
	if err != nil {
		return "", err
	}
	return replacement.OutTradeNo, nil
}

func (s *PaymentService) GetPublicCheckoutOrder(ctx context.Context, orderNumber string) (*dbent.PaymentOrder, error) {
	// Only randomly numbered checkout orders are exposed by the checkout handler.
	return s.getCheckoutOrder(ctx, paymentorder.OutTradeNoEQ(orderNumber))
}

func (s *PaymentService) getCheckoutOrder(ctx context.Context, matches ...predicate.PaymentOrder) (*dbent.PaymentOrder, error) {
	order, err := s.entClient.PaymentOrder.Query().Where(paymentorder.CheckoutKeyHashNotNil()).Where(matches...).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, infraerrors.NotFound("NOT_FOUND", "checkout order not found")
	}
	if err != nil {
		return nil, err
	}
	if order.Status == OrderStatusAwaitingConfirmation && !order.ExpiresAt.After(time.Now()) {
		if _, err := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(order.ID),
			paymentorder.StatusEQ(OrderStatusAwaitingConfirmation), paymentorder.ExpiresAtLTE(time.Now())).SetStatus(OrderStatusExpired).Save(ctx); err != nil {
			return nil, err
		}
		return s.entClient.PaymentOrder.Get(ctx, order.ID)
	}
	return order, nil
}
