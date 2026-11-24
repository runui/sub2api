package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const DefaultCompositeGroupName = "default"

var (
	ErrDefaultCompositeGroupUnavailable = infraerrors.Conflict("DEFAULT_COMPOSITE_GROUP_UNAVAILABLE", "the default group must be an active composite group")
	ErrDefaultAPIKeyUnsupported         = infraerrors.Conflict("DEFAULT_API_KEY_UNSUPPORTED", "the default composite API key is not supported in simple mode")
)

// DefaultAPIKeyRepository scopes the group lookup, first-key lookup and service
// callback to the same transaction. A nil key from firstKey means no live row.
type DefaultAPIKeyRepository interface {
	WithDefaultAPIKeyTx(ctx context.Context, userID int64, fn func(ctx context.Context, group *Group, firstKey func() (*APIKey, error)) error) error
}

func (s *APIKeyService) EnsureDefault(ctx context.Context, userID int64) (*APIKey, error) {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, ErrDefaultAPIKeyUnsupported
	}
	repo, ok := s.apiKeyRepo.(DefaultAPIKeyRepository)
	if !ok {
		return nil, fmt.Errorf("api key repository does not support default key transactions")
	}

	var key *APIKey
	var created bool
	err := repo.WithDefaultAPIKeyTx(ctx, userID, func(txCtx context.Context, group *Group, firstKey func() (*APIKey, error)) error {
		if group.Platform != PlatformComposite || !group.IsActive() {
			return ErrDefaultCompositeGroupUnavailable
		}
		user, err := s.userRepo.GetByID(txCtx, userID)
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if !s.canUserBindGroup(txCtx, user, group) {
			return ErrGroupNotAllowed
		}
		key, err = firstKey()
		if err != nil || key != nil {
			return err
		}
		token, err := s.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate key: %w", err)
		}
		key, err = s.insertAPIKey(txCtx, userID, CreateAPIKeyRequest{Name: "Default", GroupID: &group.ID}, token)
		if err != nil {
			return err
		}
		key.Group = group
		created = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created {
		// Invalidating before commit could let another instance cache a miss again.
		s.InvalidateAuthCacheByKey(ctx, key.Key)
		s.compileAPIKeyIPRules(key)
	}
	return key, nil
}
