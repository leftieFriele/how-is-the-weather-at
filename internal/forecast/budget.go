package forecast

import (
	"sync"
	"time"
)

const (
	// With a rate of 10/s, at most 5 + 10 = 15 calls fit in any second, safely under met.no's 20 req/s.
	budgetCapacity     = 5
	newLocationReserve = 2
)

// Budget is the token bucket all met.no calls share. New locations may only take a token while more than
// newLocationReserve are left, so spamming new coordinates can't stop known locations from being refreshed.
type Budget struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	tokens float64
	last   time.Time
}

// The bucket starts full.
func NewBudget(rate float64) *Budget {
	return &Budget{rate: rate, tokens: budgetCapacity, last: time.Now()}
}

// TryTake never waits.
func (b *Budget) TryTake(known bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(budgetCapacity, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now

	need := 1.0
	if !known {
		need += newLocationReserve
	}
	if b.tokens < need {
		return false
	}
	b.tokens--
	return true
}
