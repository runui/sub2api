package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const channelFalVideoRoutesKey = "fal_video_routes"

func channelFalVideoRoutes(features map[string]any) ([]config.FalVideoRoute, bool, error) {
	raw, managed := features[channelFalVideoRoutesKey]
	if !managed || raw == nil {
		return nil, managed, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, true, err
	}
	var routes []config.FalVideoRoute
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&routes); err != nil {
		return nil, true, err
	}
	return routes, true, (config.FalVideoConfig{Routes: routes}).Validate()
}

func (s *ChannelService) validateFalVideoRoutes(ctx context.Context, channel *Channel, checkReferences bool) error {
	routes, _, err := channelFalVideoRoutes(channel.FeaturesConfig)
	bad := func(message string) error { return infraerrors.BadRequest("INVALID_FAL_VIDEO_ROUTE", message) }
	if err != nil {
		return bad(err.Error())
	}
	groups, accounts := map[int64]bool{}, map[int64]bool{}
	for _, route := range routes {
		if !slices.Contains(channel.GroupIDs, route.GroupID) {
			return bad(fmt.Sprintf("fal video group %d must be linked to this channel", route.GroupID))
		}
		if !checkReferences {
			continue // Allow unrelated edits or disabling even if an old account is gone.
		}
		if !groups[route.GroupID] {
			group, err := s.groupRepo.GetByIDLite(ctx, route.GroupID)
			if err != nil && !errors.Is(err, ErrGroupNotFound) {
				return err
			}
			if group == nil || !group.IsActive() || group.IsSubscriptionType() || !group.AllowImageGeneration {
				return bad(fmt.Sprintf("fal video group %d must be active, balance-billed, and allow image/video generation", route.GroupID))
			}
			groups[route.GroupID] = true
		}
		if !accounts[route.AccountID] {
			if s.falAccounts == nil {
				return fmt.Errorf("fal account repository unavailable")
			}
			account, err := s.falAccounts.GetByID(ctx, route.AccountID)
			if err != nil && !errors.Is(err, ErrAccountNotFound) {
				return err
			}
			if account == nil || !account.IsActive() || account.Platform != PlatformFal || account.Type != AccountTypeAPIKey || !hasFalVideoAPIKey(account.Credentials) {
				return bad(fmt.Sprintf("fal video account %d must be an active fal API key account", route.AccountID))
			}
			accounts[route.AccountID] = true
		}
	}
	return nil
}

// FalVideoRoute distinguishes an unmanaged channel (legacy YAML fallback) from
// a managed channel that is disabled or has no route for the requested model.
func (s *ChannelService) FalVideoRoute(ctx context.Context, groupID int64, model string) (config.FalVideoRoute, bool, bool, error) {
	cache, err := s.loadCache(ctx)
	if err != nil {
		return config.FalVideoRoute{}, false, false, err
	}
	if cache.falLoadErr != nil {
		return config.FalVideoRoute{}, false, false, cache.falLoadErr
	}
	ch := cache.channelByGroupID[groupID]
	if ch == nil {
		return config.FalVideoRoute{}, false, false, nil
	}
	entry, managed := cache.falRoutes[ch.ID]
	if !managed || !ch.IsActive() {
		return config.FalVideoRoute{}, false, managed, nil
	}
	if entry.err != nil {
		return config.FalVideoRoute{}, false, true, entry.err
	}
	route, found := entry.routes[groupID][model]
	route.PriceUSDPerSecond = maps.Clone(route.PriceUSDPerSecond)
	return route, found, true, nil
}

type channelFalVideoCache struct {
	routes map[int64]map[string]config.FalVideoRoute
	err    error
}

func cacheFalVideoRoutes(cache *channelCache, channel *Channel) {
	routes, managed, err := channelFalVideoRoutes(channel.FeaturesConfig)
	if !managed {
		return
	}
	entry := channelFalVideoCache{routes: make(map[int64]map[string]config.FalVideoRoute), err: err}
	for _, route := range routes {
		if entry.routes[route.GroupID] == nil {
			entry.routes[route.GroupID] = make(map[string]config.FalVideoRoute)
		}
		entry.routes[route.GroupID][route.Model] = route
	}
	cache.falRoutes[channel.ID] = entry
}
