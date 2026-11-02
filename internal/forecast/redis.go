package forecast

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "weather:"

// RedisStore shares the state between all instances. Every instance must use the same Redis.
type RedisStore struct {
	rdb  *redis.Client
	rate float64
}

func NewRedisStore(rdb *redis.Client, rate float64) *RedisStore {
	return &RedisStore{rdb: rdb, rate: rate}
}

var (
	setMaxScript = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
if tonumber(ARGV[1]) > current then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
end
return 1`)

	leaseScript = redis.NewScript(`
local held = redis.call('GET', KEYS[1])
if held and tonumber(held) > tonumber(ARGV[1]) then
  return 0
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1`)

	// Same rules as Budget. The time comes from the caller; it never goes backwards, so an instance whose clock is
	// behind can't refill the bucket twice.
	takeTokenScript = redis.NewScript(`
local capacity, rate, need, now = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local state = redis.call('HMGET', KEYS[1], 'tokens', 'last')
local tokens = tonumber(state[1]) or capacity
local last = tonumber(state[2]) or now
if now > last then
  tokens = math.min(capacity, tokens + (now - last) / 1000 * rate)
  last = now
end
local taken = 0
if tokens >= need then
  tokens = tokens - 1
  taken = 1
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'last', tostring(last))
redis.call('PEXPIRE', KEYS[1], 60000)
return taken`)
)

func (s *RedisStore) get(ctx context.Context, key string) (string, bool, error) {
	v, err := s.rdb.Get(ctx, keyPrefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *RedisStore) set(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.rdb.Set(ctx, keyPrefix+key, value, ttl).Err()
}

func (s *RedisStore) del(ctx context.Context, keys ...string) error {
	prefixed := make([]string, len(keys))
	for i, k := range keys {
		prefixed[i] = keyPrefix + k
	}
	return s.rdb.Del(ctx, prefixed...).Err()
}

func (s *RedisStore) incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	var n *redis.IntCmd
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		n = p.Incr(ctx, keyPrefix+key)
		p.PExpire(ctx, keyPrefix+key, ttl)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n.Val(), nil
}

func (s *RedisStore) setMax(ctx context.Context, key string, value int64, ttl time.Duration) error {
	return setMaxScript.Run(ctx, s.rdb, []string{keyPrefix + key}, value, ttl.Milliseconds()).Err()
}

func (s *RedisStore) lease(ctx context.Context, key string, now, until time.Time) (bool, error) {
	ttl := max(until.Sub(now), time.Millisecond)
	n, err := leaseScript.Run(ctx, s.rdb, []string{keyPrefix + key}, now.UnixMilli(), until.UnixMilli(), ttl.Milliseconds()).Int()
	return n == 1, err
}

func (s *RedisStore) takeToken(ctx context.Context, now time.Time, known bool) (bool, error) {
	need := 1
	if !known {
		need += newLocationReserve
	}
	n, err := takeTokenScript.Run(ctx, s.rdb, []string{keyPrefix + "budget"},
		budgetCapacity, strconv.FormatFloat(s.rate, 'f', -1, 64), need, now.UnixMilli()).Int()
	return n == 1, err
}

func (s *RedisStore) ping(ctx context.Context) error { return s.rdb.Ping(ctx).Err() }
