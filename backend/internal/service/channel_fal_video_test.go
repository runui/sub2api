//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type falChannelTestGroups struct {
	GroupRepository
	group *Group
}

func (r falChannelTestGroups) GetByIDLite(context.Context, int64) (*Group, error) {
	return r.group, nil
}

func TestChannelFalVideoSaveUpdatesRouting(t *testing.T) {
	ctx := context.Background()
	var saved *Channel
	save := func(ch *Channel) error {
		saved = ch.Clone()
		data, err := json.Marshal(ch.FeaturesConfig)
		if err != nil {
			return err
		}
		saved.FeaturesConfig = nil
		return json.Unmarshal(data, &saved.FeaturesConfig)
	}
	repo := &mockChannelRepository{
		createFn:  func(_ context.Context, ch *Channel) error { ch.ID = 1; return save(ch) },
		updateFn:  func(_ context.Context, ch *Channel) error { return save(ch) },
		getByIDFn: func(context.Context, int64) (*Channel, error) { return saved.Clone(), nil },
		getGroupPlatformsFn: func(context.Context, []int64) (map[int64]string, error) {
			return map[int64]string{9: PlatformComposite}, nil
		},
		listAllFn: func(context.Context) ([]Channel, error) {
			if saved == nil {
				return nil, nil
			}
			return []Channel{*saved.Clone()}, nil
		},
	}
	group := &Group{ID: 9, Platform: PlatformComposite, Status: StatusActive, AllowImageGeneration: true}
	account := &Account{ID: 4, Platform: PlatformFal, Status: StatusActive, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}
	channels := ProvideChannelService(repo, falChannelTestGroups{group: group}, nil, nil, nil, falVideoTestAccounts{account: account})
	legacy := config.FalVideoRoute{GroupID: 9, AccountID: 8, Model: "video", PriceUSDPerSecond: map[string]float64{"720p": 5}}
	video := &FalVideoService{channels: channels, routes: map[int64]map[string]config.FalVideoRoute{9: {"video": legacy, "old": legacy}}}
	check := func(model string, accountID int64, rate float64) {
		t.Helper()
		route, found, err := video.ResolveRoute(ctx, 9, model)
		require.NoError(t, err)
		require.Equal(t, accountID > 0, found)
		require.Equal(t, accountID, route.AccountID)
		require.Equal(t, rate, route.PriceUSDPerSecond["720p"])
	}
	check("video", 8, 5)
	managed := legacy
	managed.AccountID = 4
	managed.PriceUSDPerSecond = map[string]float64{"720p": 0.15}
	input := &CreateChannelInput{Name: "Media", GroupIDs: []int64{9}, FeaturesConfig: map[string]any{channelFalVideoRoutesKey: []config.FalVideoRoute{managed}}}
	_, err := channels.Create(ctx, input)
	require.NoError(t, err)
	check("video", 4, 0.15)
	check("old", 0, 0)
	resolved, _, err := video.ResolveRoute(ctx, 9, "video")
	require.NoError(t, err)
	resolved.PriceUSDPerSecond["720p"] = 10
	check("video", 4, 0.15)

	_, err = channels.Update(ctx, 1, &UpdateChannelInput{FeaturesConfig: map[string]any{"other": true}})
	require.NoError(t, err)
	check("video", 4, 0.15)
	_, err = channels.Update(ctx, 1, &UpdateChannelInput{Status: StatusDisabled})
	require.NoError(t, err)
	check("video", 0, 0)
	_, err = channels.Update(ctx, 1, &UpdateChannelInput{Status: StatusActive})
	require.NoError(t, err)
	check("video", 4, 0.15)
	_, err = channels.Update(ctx, 1, &UpdateChannelInput{FeaturesConfig: map[string]any{channelFalVideoRoutesKey: []any{}}})
	require.NoError(t, err)
	check("video", 0, 0)
}

func TestChannelFalVideoRejectsInvalidRoutesAndCacheErrors(t *testing.T) {
	ctx := context.Background()
	group := &Group{ID: 9, Status: StatusActive, AllowImageGeneration: true}
	repo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) { return nil, errors.New("database down") }}
	account := &Account{ID: 4, Platform: PlatformGrok, Status: StatusActive, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}
	s := ProvideChannelService(repo, falChannelTestGroups{group: group}, nil, nil, nil, falVideoTestAccounts{account: account})
	for _, raw := range []string{
		`[{"group_id":10,"account_id":4,"model":"video","price_usd_per_second":{"720p":0.1}}]`,
		`[{"group_id":9,"account_id":4,"model":"video","price_usd_per_second":{"480p":0.1}}]`,
		`[{"group_id":9,"account_id":4,"model":"video","price_usd_per_second":{"720p":0.1}}]`,
	} {
		_, err := s.Create(ctx, &CreateChannelInput{Name: "Media", GroupIDs: []int64{9}, FeaturesConfig: map[string]any{channelFalVideoRoutesKey: json.RawMessage(raw)}})
		require.Equal(t, "INVALID_FAL_VIDEO_ROUTE", infraerrors.Reason(err), "%v", err)
	}
	video := &FalVideoService{channels: s}
	for range 2 {
		_, _, err := video.ResolveRoute(ctx, 9, "video")
		require.ErrorIs(t, err, ErrFalVideoTemporarilyUnavailable)
	}
}
