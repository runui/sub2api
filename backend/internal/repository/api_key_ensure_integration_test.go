//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lib/pq"
)

func newDefaultKeyTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("default_keys_%d", time.Now().UnixNano())
	_, err := integrationDB.Exec("CREATE DATABASE " + pq.QuoteIdentifier(name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec("DROP DATABASE " + pq.QuoteIdentifier(name))
		if err != nil {
			t.Error(err)
		}
	})
	dsn, err := url.Parse(integrationDSN)
	require.NoError(t, err)
	dsn.Path = "/" + name
	db, err := sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDefaultCompositeGroupMigrationPreservesExistingInstall(t *testing.T) {
	db := newDefaultKeyTestDatabase(t)
	ctx := context.Background()
	require.NoError(t, ApplyMigrationsForRunMode(ctx, db, config.RunModeSimple))
	var platform string
	require.NoError(t, db.QueryRow(`SELECT platform FROM groups WHERE name = 'default'`).Scan(&platform))
	require.Equal(t, service.PlatformAnthropic, platform)

	_, err := db.Exec(`UPDATE groups SET description = 'admin-owned' WHERE name = 'default'`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db))
	var description string
	require.NoError(t, db.QueryRow(`SELECT platform, description FROM groups WHERE name = 'default'`).Scan(&platform, &description))
	require.Equal(t, service.PlatformAnthropic, platform)
	require.Equal(t, "admin-owned", description)
}

func TestEnsureDefaultAPIKeyPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := newDefaultKeyTestDatabase(t)
	require.NoError(t, ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := NewAPIKeyRepository(client, db)
	users := NewUserRepository(client, db)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	newService := func() *service.APIKeyService {
		return service.NewAPIKeyService(repo, users, NewGroupRepository(client, db), NewUserSubscriptionRepository(client), nil, nil, cfg)
	}
	svc := newService()
	defaultGroups := func() *dbent.GroupQuery {
		return client.Group.Query().Where(group.NameEQ(service.DefaultCompositeGroupName), group.DeletedAtIsNil())
	}
	seed, err := defaultGroups().Only(ctx)
	require.NoError(t, err)
	require.Equal(t, service.PlatformComposite, seed.Platform)
	require.Equal(t, service.StatusActive, seed.Status)
	// A soft-deleted seed must not prevent creation of a replacement.
	require.NoError(t, client.Group.UpdateOneID(seed.ID).SetDeletedAt(time.Now()).Exec(ctx))

	userA := mustCreateUser(t, client, &service.User{Balance: 10})
	userB := mustCreateUser(t, client, &service.User{})
	legacy := &service.APIKey{UserID: userA.ID, GroupID: nil, Name: "legacy", Key: "legacy_ensure_test_key", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, legacy))
	cfg.APIKeyCreate.MaxActivePerUser = 1
	_, err = svc.EnsureDefault(ctx, userA.ID)
	require.ErrorIs(t, err, service.ErrAPIKeyCountExceeded)
	exists, err := defaultGroups().Exist(ctx)
	require.NoError(t, err)
	require.False(t, exists, "a failed issuance must roll back the newly inserted group")
	cfg.APIKeyCreate.MaxActivePerUser = 0
	const requests = 12
	var keys [requests]*service.APIKey
	var errs [requests]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range keys {
		wg.Go(func() {
			<-start
			userIDs := [2]int64{userA.ID, userB.ID}
			keys[i], errs[i] = newService().EnsureDefault(ctx, userIDs[i%2])
		})
	}
	close(start)
	wg.Wait()
	for i, key := range keys {
		require.NoError(t, errs[i])
		require.NotNil(t, key)
		require.Equal(t, keys[i%2].ID, key.ID)
		require.Equal(t, keys[i%2].Key, key.Key)
	}
	require.NotEqual(t, legacy.ID, keys[0].ID)
	require.NotEqual(t, keys[0].ID, keys[1].ID)
	g, err := defaultGroups().Only(ctx)
	require.NoError(t, err, "exactly one live default group")
	require.NotEqual(t, seed.ID, g.ID)
	require.Equal(t, service.PlatformComposite, g.Platform)
	require.Equal(t, g.ID, *keys[0].GroupID)
	count, err := client.APIKey.Query().Where(apikey.GroupIDEQ(g.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	firstID := keys[0].ID
	require.NoError(t, client.APIKey.UpdateOneID(firstID).SetStatus(service.StatusDisabled).SetExpiresAt(time.Now().Add(-time.Hour)).Exec(ctx))
	later := &service.APIKey{UserID: userA.ID, GroupID: &g.ID, Name: "later", Key: "later_ensure_test_key", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, later))
	cfg.APIKeyCreate.MaxActivePerUser = 1
	first, err := svc.EnsureDefault(ctx, userA.ID)
	require.NoError(t, err, "existing keys bypass creation limits")
	require.Equal(t, firstID, first.ID)
	require.Equal(t, service.StatusDisabled, first.Status)
	require.True(t, first.ExpiresAt.Before(time.Now()))
	require.NoError(t, client.APIKey.UpdateOneID(firstID).SetDeletedAt(time.Now()).Exec(ctx))
	first, err = svc.EnsureDefault(ctx, userA.ID)
	require.NoError(t, err)
	require.Equal(t, later.ID, first.ID)
	userC := mustCreateUser(t, client, &service.User{})
	require.NoError(t, repo.Create(ctx, &service.APIKey{UserID: userC.ID, Name: "legacy", Key: "legacy_key_for_user_c", Status: service.StatusActive}))
	_, err = svc.EnsureDefault(ctx, userC.ID)
	require.ErrorIs(t, err, service.ErrAPIKeyCountExceeded)

	require.NoError(t, client.Group.UpdateOneID(g.ID).SetIsExclusive(true).Exec(ctx))
	_, err = svc.EnsureDefault(ctx, userA.ID)
	require.ErrorIs(t, err, service.ErrGroupNotAllowed)
	require.NoError(t, client.Group.UpdateOneID(g.ID).SetIsExclusive(false).SetStatus(service.StatusDisabled).Exec(ctx))
	_, err = svc.EnsureDefault(ctx, userA.ID)
	require.ErrorIs(t, err, service.ErrDefaultCompositeGroupUnavailable)
	require.NoError(t, client.Group.UpdateOneID(g.ID).SetStatus(service.StatusActive).SetPlatform(service.PlatformAnthropic).Exec(ctx))
	_, err = svc.EnsureDefault(ctx, userA.ID)
	require.ErrorIs(t, err, service.ErrDefaultCompositeGroupUnavailable)

	// The existing key keeps the same group ID across admin changes, and the
	// resolver can use that ID for routes targeting different providers.
	require.NoError(t, client.Group.UpdateOneID(g.ID).SetPlatform(service.PlatformComposite).Exec(ctx))
	routeRepo := NewCompositeModelRouteRepository(client)
	resolver := service.NewCompositeRouteResolver(routeRepo)
	for model, platform := range map[string]string{"public-a": service.PlatformOpenAI, "public-b": service.PlatformAnthropic} {
		require.NoError(t, routeRepo.Create(ctx, &service.CompositeModelRoute{
			GroupID: g.ID, PublicModel: model, UpstreamModel: "upstream-" + model, TargetPlatform: platform,
			MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointAny, Enabled: true,
		}))
		decision, err := resolver.Resolve(ctx, *later.GroupID, model, service.CompositeRouteEndpointChatCompletions)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, platform, decision.TargetPlatform)
	}
	models, err := resolver.ListExactPublicModels(ctx, *later.GroupID, service.CompositeRouteEndpointChatCompletions, true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"public-a", "public-b"}, models)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Use the production route registration; the test login middleware supplies
	// the authenticated subject without needing a separate sign-in fixture.
	routesForUser := func(router *gin.Engine, serviceForRoute *service.APIKeyService) {
		handlers := &handler.Handlers{APIKey: handler.NewAPIKeyHandler(serviceForRoute)}
		routes.RegisterUserRoutes(router.Group("/api/v1"), handlers, func(c *gin.Context) {
			if c.GetHeader("Authorization") != "Bearer test-login" {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userA.ID})
			c.Next()
		}, func(c *gin.Context) { c.Next() }, nil, nil)
	}
	routesForUser(router, svc)
	request := func(router *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	w := request(router, http.MethodPost, "/api/v1/keys/ensure", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w = request(router, http.MethodPost, "/api/v1/keys/ensure", "test-login")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var response struct {
		Data struct {
			ID  int64  `json:"id"`
			Key string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, later.ID, response.Data.ID)
	require.Equal(t, later.Key, response.Data.Key)

	simpleRouter := gin.New()
	routesForUser(simpleRouter, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}))
	w = request(simpleRouter, http.MethodPost, "/api/v1/keys/ensure", "test-login")
	require.Equal(t, http.StatusConflict, w.Code)
	require.Contains(t, w.Body.String(), "DEFAULT_API_KEY_UNSUPPORTED")

	accountRepo := NewAccountRepository(client, db, nil)
	for model, platform := range map[string]string{"public-a": service.PlatformOpenAI, "public-b": service.PlatformAnthropic} {
		account := mustCreateAccount(t, client, &service.Account{
			Name: "account-" + model, Platform: platform, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"upstream-*": "upstream-*"}},
		})
		require.NoError(t, accountRepo.BindGroups(ctx, account.ID, []int64{g.ID}))
	}
	gateway := service.NewGatewayService(accountRepo, nil, nil, nil, nil, nil, nil, nil,
		cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, resolver, nil, nil)
	gatewayHandler := handler.NewGatewayHandler(gateway, nil, nil, nil, nil, nil, nil, nil, svc, nil, nil, nil, nil, cfg, nil)
	router.GET("/v1/models", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(svc, nil, cfg)), gatewayHandler.Models)
	w = request(router, http.MethodGet, "/v1/models", response.Data.Key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &catalog))
	var publicIDs []string
	for _, model := range catalog.Data {
		publicIDs = append(publicIDs, model.ID)
	}
	require.ElementsMatch(t, []string{"public-a", "public-b"}, publicIDs)
}
