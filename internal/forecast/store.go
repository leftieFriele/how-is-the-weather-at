package forecast

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// Store holds the state the met.no rules depend on: forecasts, the upstream budget, pauses, leases and backoff.
// RedisStore shares it between instances; MemoryStore keeps it in one process, for tests and local runs.
//
// Each method is atomic on its own. Times are passed in rather than read from the store's clock, and expiry is
// decided by comparing stored times; ttl only lets the store forget keys nobody needs any more.
type Store interface {
	get(ctx context.Context, key string) (string, bool, error)
	set(ctx context.Context, key, value string, ttl time.Duration) error
	del(ctx context.Context, keys ...string) error
	incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// setMax stores value unless the stored value is larger.
	setMax(ctx context.Context, key string, value int64, ttl time.Duration) error
	// lease stores until and reports true, unless the key holds a time after now.
	lease(ctx context.Context, key string, now, until time.Time) (bool, error)
	takeToken(ctx context.Context, now time.Time, known bool) (bool, error)
	ping(ctx context.Context) error
}

// MemoryStore must only be used by a single instance: several would each have their own budget.
type MemoryStore struct {
	budget *Budget

	mu   sync.Mutex
	keys *simplelru.LRU[string, string]
}

// NewMemoryStore keeps the state of up to size locations.
func NewMemoryStore(rate float64, size int) (*MemoryStore, error) {
	// Each location may have a forecast, a lease and a backoff key, next to a few global ones.
	keys, err := simplelru.NewLRU[string, string](4*size, nil)
	if err != nil {
		return nil, fmt.Errorf("forecast: %w", err)
	}
	return &MemoryStore{budget: NewBudget(rate), keys: keys}, nil
}

func (s *MemoryStore) get(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.keys.Get(key)
	return v, ok, nil
}

func (s *MemoryStore) set(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys.Add(key, value)
	return nil
}

func (s *MemoryStore) del(_ context.Context, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.keys.Remove(k)
	}
	return nil
}

func (s *MemoryStore) incr(_ context.Context, key string, _ time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.keys.Get(key)
	n, _ := strconv.ParseInt(v, 10, 64)
	n++
	s.keys.Add(key, strconv.FormatInt(n, 10))
	return n, nil
}

func (s *MemoryStore) setMax(_ context.Context, key string, value int64, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.keys.Get(key)
	if current, _ := strconv.ParseInt(v, 10, 64); value > current {
		s.keys.Add(key, strconv.FormatInt(value, 10))
	}
	return nil
}

func (s *MemoryStore) lease(_ context.Context, key string, now, until time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.keys.Get(key)
	if held, _ := strconv.ParseInt(v, 10, 64); held > now.UnixMilli() {
		return false, nil
	}
	s.keys.Add(key, strconv.FormatInt(until.UnixMilli(), 10))
	return true, nil
}

// The budget keeps its own time, which is the same clock in a single process.
func (s *MemoryStore) takeToken(_ context.Context, _ time.Time, known bool) (bool, error) {
	return s.budget.TryTake(known), nil
}

func (s *MemoryStore) ping(context.Context) error { return nil }
