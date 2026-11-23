package handler_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

type otpTestEncryptor struct{}

func (otpTestEncryptor) Encrypt(value string) (string, error) { return value, nil }
func (otpTestEncryptor) Decrypt(value string) (string, error) { return value, nil }

type emailOTPFixture struct {
	t        *testing.T
	router   *gin.Engine
	db       *dbent.Client
	settings service.SettingRepository
	users    service.UserRepository
	cache    service.EmailOTPCache
	clock    *miniredis.Miniredis
	mail     <-chan string
	request  int
}

func newEmailOTPFixture(t *testing.T) *emailOTPFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	// Ent does not manage this migration-backed table, which GetByID reads.
	_, err = db.Exec(`CREATE TABLE user_avatars (
		user_id INTEGER PRIMARY KEY, storage_provider TEXT NOT NULL,
		storage_key TEXT NOT NULL DEFAULT '', url TEXT NOT NULL,
		content_type TEXT NOT NULL DEFAULT '', byte_size INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT NOT NULL DEFAULT '', updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })
	clock := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: clock.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := &config.Config{JWT: config.JWTConfig{Secret: "test-email-otp-key", ExpireHour: 1, RefreshTokenExpireDays: 7}}
	settings := repository.NewSettingRepository(client)
	settingSvc := service.NewSettingService(settings, cfg)
	users := repository.NewUserRepository(client, db)
	email := service.NewEmailService(settings, repository.NewEmailCache(rdb))
	queue := service.NewEmailQueueService(email, 1)
	cache := repository.NewEmailOTPCache(rdb)
	port, inbox := startEmailOTPSMTP(t)
	t.Cleanup(queue.Stop)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{
		service.SettingKeyRegistrationEnabled: "true",
		service.SettingKeySMTPHost:            "localhost",
		service.SettingKeySMTPPort:            strconv.Itoa(port),
		service.SettingKeySMTPUsername:        "test-user",
		service.SettingKeySMTPPassword:        "test-password",
		service.SettingKeySMTPFrom:            "site@example.com",
		service.SettingKeyDefaultBalance:      "7",
	}))
	userSvc := service.NewUserService(users, settings, nil, nil)
	totpSvc := service.NewTotpService(users, otpTestEncryptor{}, repository.NewTotpCache(rdb), settingSvc, email, queue)
	authSvc := service.ProvideAuthService(client, users, repository.NewRedeemCodeRepository(client), repository.NewRefreshTokenCache(rdb), cfg, settingSvc, email, nil, nil, nil, queue, cache, nil, nil, nil, nil, nil)
	auth := handler.NewAuthHandler(cfg, authSvc, userSvc, settingSvc, nil, nil, totpSvc, nil)
	admin := adminhandler.NewSettingHandler(settingSvc, email, nil, nil, nil, nil, nil)
	router := gin.New()
	v1 := router.Group("/api/v1")
	noOp := func(c *gin.Context) { c.Next() }
	routes.RegisterAuthRoutes(v1, &handler.Handlers{Auth: auth, Setting: &handler.SettingHandler{}}, noOp, noOp, rdb, settingSvc, nil)
	// Test the settings handler independently of the existing admin auth guard.
	v1.GET("/admin/settings/email-otp", admin.GetEmailOTPSettings)
	v1.PUT("/admin/settings/email-otp", admin.UpdateEmailOTPSettings)
	f := &emailOTPFixture{t: t, router: router, db: client, settings: settings, users: users, cache: cache, clock: clock, mail: inbox}
	require.Equal(t, false, f.call(http.MethodGet, "/admin/settings/email-otp", "", 200).Data["enabled"])
	require.Equal(t, "EMAIL_OTP_DISABLED", f.post("/send", `{"email":"missing@example.com"}`, 403).Reason)
	require.Equal(t, true, f.call(http.MethodPut, "/admin/settings/email-otp", `{"enabled":true}`, 200).Data["enabled"])
	return f
}

type emailOTPFixtureResponse struct {
	Reason string         `json:"reason"`
	Data   map[string]any `json:"data"`
}

func (f *emailOTPFixture) call(method, path, body string, status int) emailOTPFixtureResponse {
	f.t.Helper()
	f.request++
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, method, "/api/v1"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = fmt.Sprintf("203.0.113.%d:12345", f.request)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	require.Equal(f.t, status, w.Code, "%s %s: %s", method, path, w.Body.String())
	var result emailOTPFixtureResponse
	require.NoError(f.t, json.Unmarshal(w.Body.Bytes(), &result))
	return result
}

func (f *emailOTPFixture) post(path, body string, status int) emailOTPFixtureResponse {
	f.t.Helper()
	return f.call(http.MethodPost, "/auth/email-otp"+path, body, status)
}

func (f *emailOTPFixture) setting(key, value string) {
	f.t.Helper()
	require.NoError(f.t, f.settings.Set(context.Background(), key, value))
}

func (f *emailOTPFixture) createUser(email, role, status string) *service.User {
	f.t.Helper()
	user := &service.User{Email: email, Role: role, Status: status, PasswordHash: "test-hash", Concurrency: 1}
	require.NoError(f.t, f.users.Create(context.Background(), user))
	return user
}

func (f *emailOTPFixture) send(email string) string {
	f.t.Helper()
	data := f.post("/send", fmt.Sprintf(`{"email":%q}`, email), 202).Data
	require.Equal(f.t, float64(300), data["expires_in"])
	require.Equal(f.t, float64(60), data["retry_after"])
	require.Len(f.t, data, 3)
	return data["challenge_id"].(string)
}

var emailOTPTestCodePattern = regexp.MustCompile(`<strong>([0-9]{6})</strong>`)

func (f *emailOTPFixture) challenge(email string) (string, string) {
	f.t.Helper()
	id := f.send(email)
	select {
	case message := <-f.mail:
		match := emailOTPTestCodePattern.FindStringSubmatch(message)
		require.Len(f.t, match, 2)
		// The server receives SMTP's final 250 before activating the code in Redis.
		require.Eventually(f.t, func() bool {
			for _, key := range f.clock.Keys() {
				if f.clock.HGet(key, "id") == id {
					return f.clock.HGet(key, "delivered") == "1"
				}
			}
			return false
		}, 2*time.Second, time.Millisecond)
		return id, match[1]
	case <-time.After(3 * time.Second):
		f.t.Fatal("no email received from the OTP queue")
		return "", ""
	}
}

func otpVerifyBody(email, id, code string) string {
	return fmt.Sprintf(`{"email":%q,"challenge_id":%q,"code":%q}`, email, id, code)
}

func TestEmailOTPAPILoginAndAutomaticRegistration(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	f.setting(service.SettingKeyEmailVerifyEnabled, "true")
	const email = "NewMailbox@example.com"
	id, code := f.challenge(email)
	body := otpVerifyBody(strings.ToLower(email), id, code)
	result := f.post("/verify", body, 200).Data
	require.NotEmpty(t, result["access_token"])
	require.NotEmpty(t, result["refresh_token"])
	created, err := f.users.GetByEmail(ctx, email)
	require.NoError(t, err)
	require.Equal(t, email, created.Email)
	require.EqualValues(t, 7, created.Balance)
	require.NotEmpty(t, created.PasswordHash)
	require.Equal(t, "EMAIL_OTP_INVALID", f.post("/verify", body, 400).Reason)
	require.Equal(t, "EMAIL_OTP_INVALID", f.post("/verify", `{}`, 400).Reason)

	f.setting(service.SettingKeyRegistrationEnabled, "false")
	f.clock.FastForward(service.EmailOTPResendCooldown)
	id, code = f.challenge(email)
	result = f.post("/verify", otpVerifyBody(strings.ToUpper(email), id, code), 200).Data
	require.EqualValues(t, created.ID, result["user"].(map[string]any)["id"])
	count, err := f.db.User.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestEmailOTPAPIInvitationRegistration(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	f.setting(service.SettingKeyInvitationCodeEnabled, "true")
	invitation, err := f.db.RedeemCode.Create().SetCode("test-invite").SetType(service.RedeemTypeInvitation).SetStatus(service.StatusUnused).SetValue(0).Save(ctx)
	require.NoError(t, err)
	const email = "invitee@example.com"
	id, code := f.challenge(email)
	verify := fmt.Sprintf(`{"email":%q,"challenge_id":%q,"code":%q,"invitation_code":"bad"}`, email, id, code)
	result := f.post("/verify", verify, 200).Data
	require.Equal(t, true, result["requires_invitation"])
	require.Equal(t, "INVITATION_CODE_INVALID", result["invitation_error"])
	require.NotContains(t, result, "access_token")
	grant := result["registration_token"].(string)
	_, err = f.users.GetByEmail(ctx, email)
	require.ErrorIs(t, err, service.ErrUserNotFound)
	require.Equal(t, "INVITATION_CODE_INVALID", f.post("/complete-registration", fmt.Sprintf(`{"registration_token":%q,"invitation_code":"bad"}`, grant), 400).Reason)
	body := fmt.Sprintf(`{"registration_token":%q,"invitation_code":"test-invite","email":"other@example.com"}`, grant)
	result = f.post("/complete-registration", body, 200).Data
	require.NotEmpty(t, result["access_token"])
	require.Equal(t, email, result["user"].(map[string]any)["email"])
	require.Equal(t, "EMAIL_OTP_REGISTRATION_INVALID", f.post("/complete-registration", body, 400).Reason)
	used, err := f.db.RedeemCode.Get(ctx, invitation.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusUsed, used.Status)
	require.EqualValues(t, *used.UsedBy, result["user"].(map[string]any)["id"])
}

func TestEmailOTPAPIRechecksRegistrationPolicy(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	const email = "policy@example.com"
	id, code := f.challenge(email)
	// Policy can change after the code was delivered, not only before send.
	f.setting(service.SettingKeyInvitationCodeEnabled, "true")
	result := f.post("/verify", otpVerifyBody(email, id, code), 200).Data
	require.Equal(t, true, result["requires_invitation"])
	grant := result["registration_token"].(string)
	body := fmt.Sprintf(`{"registration_token":%q,"invitation_code":"test-invite"}`, grant)
	f.setting(service.SettingKeyRegistrationEnabled, "false")
	f.post("/complete-registration", body, 403)
	_, err := f.users.GetByEmail(ctx, email)
	require.ErrorIs(t, err, service.ErrUserNotFound)

	f.setting(service.SettingKeyRegistrationEnabled, "true")
	f.clock.FastForward(service.EmailOTPRegistrationTTL)
	require.Equal(t, "EMAIL_OTP_REGISTRATION_INVALID", f.post("/complete-registration", body, 400).Reason)
}

func TestEmailOTPAPIOnlyOneAccountCanClaimAnInvitation(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	f.setting(service.SettingKeyInvitationCodeEnabled, "true")
	invitation, err := f.db.RedeemCode.Create().SetCode("shared-invite").SetType(service.RedeemTypeInvitation).SetStatus(service.StatusUnused).SetValue(0).Save(ctx)
	require.NoError(t, err)
	results := make(chan int, 2)
	start := make(chan struct{})
	var requests []*http.Request
	for _, email := range []string{"first@example.com", "second@example.com"} {
		id, code := f.challenge(email)
		data := f.post("/verify", otpVerifyBody(email, id, code), 200).Data
		body := fmt.Sprintf(`{"registration_token":%q,"invitation_code":"shared-invite"}`, data["registration_token"])
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email-otp/complete-registration", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		requests = append(requests, request)
	}
	for _, request := range requests {
		go func() {
			<-start
			w := httptest.NewRecorder()
			f.router.ServeHTTP(w, request)
			results <- w.Code
		}()
	}
	close(start)
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusBadRequest}, []int{<-results, <-results})
	user, err := f.db.User.Query().Only(ctx)
	require.NoError(t, err, "the losing request must not leave a user behind")
	used, err := f.db.RedeemCode.Get(ctx, invitation.ID)
	require.NoError(t, err)
	require.NotNil(t, used.UsedBy)
	require.Equal(t, user.ID, *used.UsedBy)
}

func TestEmailOTPAPIRejectsStaleAccountOrRegistrationProof(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	const first, second = "existing@example.com", "raced@example.com"
	user := f.createUser(first, service.RoleUser, service.StatusActive)
	id, code := f.challenge(first)
	user.Email = "changed@example.com"
	require.NoError(t, f.users.Update(ctx, user, service.UserUpdateFields{Email: true}))
	require.Equal(t, "EMAIL_OTP_INVALID", f.post("/verify", otpVerifyBody(first, id, code), 400).Reason)

	id, code = f.challenge(second)
	f.createUser(second, service.RoleUser, service.StatusActive)
	require.Equal(t, "EMAIL_EXISTS", f.post("/verify", otpVerifyBody(second, id, code), 409).Reason)

	id, code = f.challenge(user.Email)
	f.call(http.MethodPut, "/admin/settings/email-otp", `{"enabled":false}`, 200)
	require.Equal(t, "EMAIL_OTP_DISABLED", f.post("/verify", otpVerifyBody(user.Email, id, code), 403).Reason)
	f.call(http.MethodPut, "/admin/settings/email-otp", `{"enabled":true}`, 200)
	require.Equal(t, "EMAIL_OTP_INVALID", f.post("/verify", otpVerifyBody(user.Email, id, code), 400).Reason)
}

func TestEmailOTPAPIPreservesTOTPAndChecksRoleAgain(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	const secret, email = "JBSWY3DPEHPK3PXP", "two-factor@example.com"
	user := f.createUser(email, service.RoleUser, service.StatusActive)
	require.NoError(t, f.db.User.UpdateOneID(user.ID).SetTotpEnabled(true).SetTotpSecretEncrypted(secret).Exec(ctx))
	f.setting(service.SettingKeyTotpEnabled, "false")
	startTOTP := func() string {
		id, code := f.challenge(email)
		data := f.post("/verify", otpVerifyBody(email, id, code), 200).Data
		require.Equal(t, true, data["requires_2fa"])
		require.NotContains(t, data, "access_token")
		return data["temp_token"].(string)
	}
	finish := func(token string, status int) emailOTPFixtureResponse {
		code, err := totp.GenerateCode(secret, time.Now())
		require.NoError(t, err)
		return f.call(http.MethodPost, "/auth/login/2fa", fmt.Sprintf(`{"temp_token":%q,"totp_code":%q}`, token, code), status)
	}
	token := startTOTP()
	require.NotEmpty(t, finish(token, 200).Data["access_token"])
	finish(token, 400)
	f.clock.FastForward(service.EmailOTPResendCooldown)
	token = startTOTP()
	require.NoError(t, f.db.User.UpdateOneID(user.ID).SetRole(service.RoleAdmin).Exec(ctx))
	require.Equal(t, "EMAIL_OTP_NOT_ALLOWED", finish(token, 403).Reason)
}

func TestEmailOTPAPISendDoesNotExposeIneligibleAccounts(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	f.setting(service.SettingKeyRegistrationEnabled, "false")
	f.createUser("admin@example.com", service.RoleAdmin, service.StatusActive)
	f.createUser("disabled@example.com", service.RoleUser, service.StatusDisabled)
	for _, email := range []string{"admin@example.com", "disabled@example.com", "unregistered@example.com"} {
		id := f.send(email)
		generation, err := f.settings.GetValue(ctx, service.SettingKeyEmailOTPGeneration)
		require.NoError(t, err)
		require.Eventually(f.t, func() bool {
			ttl, err := f.cache.ChallengeRemaining(ctx, email, id, generation)
			return err == nil && ttl == 0
		}, 2*time.Second, time.Millisecond)
		require.Empty(t, f.mail)
		require.Equal(t, "VERIFY_CODE_TOO_FREQUENT", f.post("/send", fmt.Sprintf(`{"email":%q}`, email), 429).Reason)
	}
}

func TestEmailOTPAPIUnsentChallengesAreDiscarded(t *testing.T) {
	f := newEmailOTPFixture(t)
	ctx := context.Background()
	f.setting(service.SettingKeyRegistrationEmailBlacklist, `["blocked@example.com"]`)
	f.setting(service.SettingKeyRegistrationEmailSuffixWhitelist, `["@example.com"]`)
	f.createUser("existing@example.com", service.RoleUser, service.StatusActive)
	// The first address is rejected by SMTP; the others are rejected before
	// delivery by alias, blacklist or domain policy, with the same response.
	for _, email := range []string{"failed-delivery@example.com", "existing+alias@example.com", "blocked+alias@example.com", "user@outside.com"} {
		id := f.send(email)
		generation, err := f.settings.GetValue(ctx, service.SettingKeyEmailOTPGeneration)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			ttl, err := f.cache.ChallengeRemaining(ctx, email, id, generation)
			return err == nil && ttl == 0
		}, 2*time.Second, time.Millisecond)
		require.Empty(t, f.mail)
		require.Equal(t, "EMAIL_OTP_INVALID", f.post("/verify", otpVerifyBody(email, id, "123456"), 400).Reason)
	}
}

// A local SMTP server captures the actual MIME body while exercising the
// production SMTP and asynchronous queue paths, without external credentials.
func startEmailOTPSMTP(t *testing.T) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	inbox := make(chan string, 16)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				r := textproto.NewReader(bufio.NewReader(conn))
				_, _ = io.WriteString(conn, "220 localhost ready\r\n")
				for {
					line, err := r.ReadLine()
					if err != nil {
						return
					}
					switch strings.Fields(line)[0] {
					case "EHLO":
						_, _ = io.WriteString(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
					case "AUTH":
						_, _ = io.WriteString(conn, "235 authenticated\r\n")
					case "RCPT":
						if strings.Contains(line, "failed-delivery@example.com") {
							_, _ = io.WriteString(conn, "550 recipient rejected\r\n")
							return
						}
						_, _ = io.WriteString(conn, "250 OK\r\n")
					case "MAIL":
						_, _ = io.WriteString(conn, "250 OK\r\n")
					case "DATA":
						_, _ = io.WriteString(conn, "354 send data\r\n")
						payload, err := r.ReadDotBytes()
						if err != nil {
							return
						}
						body, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(payload))))
						if err != nil {
							return
						}
						_, _ = io.WriteString(conn, "250 queued\r\n")
						inbox <- string(body)
					case "QUIT":
						_, _ = io.WriteString(conn, "221 bye\r\n")
						return
					default:
						_, _ = io.WriteString(conn, "500 unsupported command\r\n")
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().(*net.TCPAddr).Port, inbox
}
