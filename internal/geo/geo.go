// Package geo parses and formats coordinates.
//
// Coordinates are kept as integers in units of 1/10 000 degree, so they never have more than the 4 decimals
// the met.no Terms of Service allow, and so equal locations always give equal cache keys.
package geo

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	decimals = 4
	scale    = 10_000
)

var (
	ErrNotPlainDecimal = errors.New("must be a plain decimal number")
	ErrOutOfRange      = errors.New("is out of range")
)

// Coord is in units of 1/10 000 degree.
type Coord int32

// Point is the forecast cache key.
type Point struct {
	Lat Coord
	Lon Coord
}

// ParseLat truncates to 4 decimals.
func ParseLat(s string) (Coord, error) { return parse(s, 90) }

// ParseLon truncates to 4 decimals.
func ParseLon(s string) (Coord, error) { return parse(s, 180) }

// parse works on the text rather than a float, so truncation never suffers from rounding:
// as a float, 59.9139 may be 59.91389999…, which would truncate to 59.9138.
func parse(s string, limit int) (Coord, error) {
	digits, negative := strings.CutPrefix(s, "-")
	whole, frac, hasDot := strings.Cut(digits, ".")
	if !isDigits(whole) || (hasDot && !isDigits(frac)) {
		return 0, ErrNotPlainDecimal
	}
	if len(whole) > 3 {
		return 0, ErrOutOfRange
	}
	w, err := strconv.Atoi(whole)
	if err != nil {
		return 0, ErrNotPlainDecimal
	}
	if w > limit || (w == limit && strings.Trim(frac, "0") != "") {
		return 0, ErrOutOfRange
	}

	// Truncate towards zero: drop the digits after the 4th decimal, and pad to exactly 4.
	frac = (frac + strings.Repeat("0", decimals))[:decimals]
	f, err := strconv.Atoi(frac)
	if err != nil {
		return 0, ErrNotPlainDecimal
	}
	v := w*scale + f
	if negative {
		v = -v
	}
	return Coord(v), nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// String drops trailing zeros: "10.75", "-33.8678".
func (c Coord) String() string {
	v := int(c)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	whole, frac := v/scale, v%scale
	if frac == 0 {
		return sign + strconv.Itoa(whole)
	}
	return fmt.Sprintf("%s%d.%s", sign, whole, strings.TrimRight(fmt.Sprintf("%04d", frac), "0"))
}

func (c Coord) Float() float64 { return float64(c) / scale }
