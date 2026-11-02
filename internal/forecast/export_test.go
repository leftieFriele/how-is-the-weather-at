package forecast

import (
	"context"
	"time"

	"how-is-the-weather-at-api/internal/geo"
)

// TakeToken lets tests use up the budget.
func TakeToken(s Store, known bool) bool {
	ok, _ := s.takeToken(context.Background(), time.Now(), known)
	return ok
}

// Forget lets tests remove a cached forecast, as Redis does when a key expires or memory runs out.
func Forget(s Store, p geo.Point) { _ = s.del(context.Background(), keyForecast(p)) }
