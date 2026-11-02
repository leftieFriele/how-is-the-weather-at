package geo_test

import (
	"errors"
	"testing"

	"how-is-the-weather-at-api/internal/geo"
)

func TestCoordinates(t *testing.T) {
	t.Run("truncates to 4 decimals as text, without float rounding", func(t *testing.T) {
		tests := []struct{ given, then string }{
			{given: "59.9139", then: "59.9139"},
			{given: "63.430512", then: "63.4305"},
			{given: "10.7500", then: "10.75"},
			{given: "10", then: "10"},
			{given: "0.00005", then: "0"},
		}
		for _, tt := range tests {
			// When
			c, err := geo.ParseLat(tt.given)

			// Then
			if err != nil {
				t.Fatalf("ParseLat(%q): %v", tt.given, err)
			}
			if got := c.String(); got != tt.then {
				t.Errorf("ParseLat(%q) = %s, want %s", tt.given, got, tt.then)
			}
		}
	})

	t.Run("truncates negative values towards zero", func(t *testing.T) {
		// When
		c, err := geo.ParseLat("-33.86789")

		// Then
		if err != nil || c.String() != "-33.8678" {
			t.Errorf("ParseLat(-33.86789) = %s, %v; want -33.8678", c, err)
		}
	})

	t.Run("makes equal locations equal cache keys", func(t *testing.T) {
		// Given
		a, _ := geo.ParseLon("10.75")
		b, _ := geo.ParseLon("10.750012")

		// Then
		if a != b {
			t.Errorf("10.75 and 10.750012 give different coordinates: %d and %d", a, b)
		}
	})

	t.Run("rejects anything that is not a plain decimal number", func(t *testing.T) {
		for _, given := range []string{"5.99e1", "NaN", "Inf", "0x1p4", "+59.9", "59.", ".5", "", "59,9", " 59.9"} {
			// When
			_, err := geo.ParseLat(given)

			// Then
			if !errors.Is(err, geo.ErrNotPlainDecimal) {
				t.Errorf("ParseLat(%q) error = %v, want ErrNotPlainDecimal", given, err)
			}
		}
	})

	t.Run("rejects values out of range", func(t *testing.T) {
		tests := []struct {
			name  string
			parse func(string) (geo.Coord, error)
			given string
		}{
			{"latitude above 90", geo.ParseLat, "90.00001"},
			{"latitude below -90", geo.ParseLat, "-91"},
			{"longitude above 180", geo.ParseLon, "180.5"},
			{"too many digits", geo.ParseLon, "0180"},
		}
		for _, tt := range tests {
			if _, err := tt.parse(tt.given); !errors.Is(err, geo.ErrOutOfRange) {
				t.Errorf("%s: parse(%q) error = %v, want ErrOutOfRange", tt.name, tt.given, err)
			}
		}
	})
}
