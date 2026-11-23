package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type emailOTPCache struct {
	rdb *redis.Client
}

func NewEmailOTPCache(rdb *redis.Client) service.EmailOTPCache {
	return &emailOTPCache{rdb: rdb}
}

// The challenge uses the exact login address. Only the delivery cooldown folds
// aliases, so aliases cannot evade throttling or authenticate as another user.
func emailOTPChallengeKey(email string) string {
	return emailOTPInboxKey(email) + ":challenge:" + emailOTPKeyDigest(email)
}

func emailOTPInboxKey(email string) string {
	return "auth:email-otp:{" + emailOTPKeyDigest(service.NormalizeEmailForAliasDedup(email)) + "}"
}

func emailOTPKeyDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// KEYS: exact-address challenge, inbox cooldown (same Redis hash slot).
// ARGV: challenge id, generation, HMAC, subject JSON, challenge TTL, cooldown.
// Returns the remaining cooldown in ms, or 0 after creating the new challenge.
var issueEmailOTPScript = redis.NewScript(`
local retry = redis.call('PTTL', KEYS[2])
if retry >= 0 then return math.max(retry, 1) end
if retry == -1 then return redis.error_reply('email OTP cooldown missing expiry') end
redis.call('SET', KEYS[2], '1', 'PX', ARGV[6])
redis.call('DEL', KEYS[1])
redis.call('HSET', KEYS[1], 'id', ARGV[1], 'generation', ARGV[2],
  'mac', ARGV[3], 'subject', ARGV[4], 'wrong', 0, 'delivered', 0)
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 0
`)

// All lifecycle operations compare challenge id and generation before touching
// state. Slow workers cannot activate or delete a replacement request.
const emailOTPMatchChallengeLua = `
local state = redis.call('HMGET', KEYS[1], 'id', 'generation')
if state[1] ~= ARGV[1] or state[2] ~= ARGV[2] then return 0 end
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then return 0 end
`

var emailOTPRemainingScript = redis.NewScript(emailOTPMatchChallengeLua + `return ttl`)

var markEmailOTPDeliveredScript = redis.NewScript(emailOTPMatchChallengeLua + `
redis.call('HSET', KEYS[1], 'delivered', 1)
return 1
`)

var discardEmailOTPScript = redis.NewScript(emailOTPMatchChallengeLua + `
-- Keep the independent cooldown even on delivery failure or policy rejection.
return redis.call('DEL', KEYS[1])
`)

// ARGV: challenge id, generation, candidate HMAC, max wrong attempts. Match
// and delete in the same script: separate GET/DEL can let two requests through.
// The subject is opaque JSON here so Lua never rounds a 64-bit user ID.
var consumeEmailOTPScript = redis.NewScript(emailOTPMatchChallengeLua + `
local data = redis.call('HMGET', KEYS[1], 'delivered', 'mac', 'subject')
if data[1] ~= '1' then return 0 end
if data[2] == ARGV[3] then
  redis.call('DEL', KEYS[1])
  return data[3]
end
local wrong = redis.call('HINCRBY', KEYS[1], 'wrong', 1)
if wrong >= tonumber(ARGV[4]) then redis.call('DEL', KEYS[1]) end
return 0
`)

func (c *emailOTPCache) Issue(ctx context.Context, email string, challenge service.EmailOTPChallenge, ttl, cooldown time.Duration) (time.Duration, error) {
	subject, err := json.Marshal(challenge.Subject)
	if err != nil {
		return 0, err
	}
	keys := []string{emailOTPChallengeKey(email), emailOTPInboxKey(email) + ":cooldown"}
	retry, err := issueEmailOTPScript.Run(ctx, c.rdb, keys, challenge.ID, challenge.Generation, challenge.CodeMAC, subject, ttl.Milliseconds(), cooldown.Milliseconds()).Int64()
	return time.Duration(retry) * time.Millisecond, err
}

func (c *emailOTPCache) ChallengeRemaining(ctx context.Context, email, id, generation string) (time.Duration, error) {
	ttl, err := emailOTPRemainingScript.Run(ctx, c.rdb, []string{emailOTPChallengeKey(email)}, id, generation).Int64()
	return time.Duration(ttl) * time.Millisecond, err
}

func (c *emailOTPCache) MarkDelivered(ctx context.Context, email, id, generation string) error {
	return markEmailOTPDeliveredScript.Run(ctx, c.rdb, []string{emailOTPChallengeKey(email)}, id, generation).Err()
}

func (c *emailOTPCache) Discard(ctx context.Context, email, id, generation string) error {
	return discardEmailOTPScript.Run(ctx, c.rdb, []string{emailOTPChallengeKey(email)}, id, generation).Err()
}

func (c *emailOTPCache) Consume(ctx context.Context, email, id, generation, codeMAC string, maxWrong int) (*service.EmailOTPSubject, error) {
	result, err := consumeEmailOTPScript.Run(ctx, c.rdb, []string{emailOTPChallengeKey(email)}, id, generation, codeMAC, maxWrong).Result()
	if err != nil {
		return nil, err
	}
	switch value := result.(type) {
	case int64:
		return nil, nil
	case string:
		var subject service.EmailOTPSubject
		if err := json.Unmarshal([]byte(value), &subject); err != nil {
			return nil, err
		}
		return &subject, nil
	default:
		return nil, errors.New("unexpected email OTP verification result")
	}
}

const emailOTPRegistrationPrefix = "auth:email-otp:registration:"

func (c *emailOTPCache) StoreRegistration(ctx context.Context, tokenHash string, grant service.EmailOTPRegistration, ttl time.Duration) error {
	data, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, emailOTPRegistrationPrefix+tokenHash, data, ttl).Err()
}

func (c *emailOTPCache) GetRegistration(ctx context.Context, tokenHash string) (*service.EmailOTPRegistration, error) {
	data, err := c.rdb.Get(ctx, emailOTPRegistrationPrefix+tokenHash).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var grant service.EmailOTPRegistration
	if err := json.Unmarshal(data, &grant); err != nil {
		return nil, err
	}
	return &grant, nil
}

func (c *emailOTPCache) ConsumeRegistration(ctx context.Context, tokenHash string) (bool, error) {
	// The key is the hash of an immutable, unguessable bearer token. A delete
	// count doubles as the claim result, supported by only one concurrent call.
	deleted, err := c.rdb.Del(ctx, emailOTPRegistrationPrefix+tokenHash).Result()
	return deleted == 1, err
}
