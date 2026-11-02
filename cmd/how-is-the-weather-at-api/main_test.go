package main

import (
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	t.Run("refuses to start without a User-Agent", func(t *testing.T) {
		// When
		_, err := parseConfig(nil)

		// Then
		if err == nil || !strings.Contains(err.Error(), "-user-agent is required") {
			t.Errorf("error = %v, want -user-agent is required", err)
		}
	})

	t.Run("refuses to start without saying where the shared state is", func(t *testing.T) {
		for _, args := range [][]string{
			{"-user-agent", "test test@example.com"},
			{"-user-agent", "test test@example.com", "-in-memory", "-redis-url", "redis://localhost:6379"},
		} {
			// When
			_, err := parseConfig(args)

			// Then
			if err == nil || !strings.Contains(err.Error(), "give exactly one of -redis-url") {
				t.Errorf("parseConfig(%q) error = %v, want exactly one of -redis-url and -in-memory", args, err)
			}
		}
	})

	t.Run("refuses an upstream rate that could exceed met.no's 20 req/s", func(t *testing.T) {
		// When
		_, err := parseConfig([]string{"-in-memory", "-user-agent", "test test@example.com", "-upstream-rate", "16"})

		// Then
		if err == nil || !strings.Contains(err.Error(), "-upstream-rate must be above 0 and at most 15") {
			t.Errorf("error = %v, want the rate to be refused", err)
		}
	})

	t.Run("takes the port from -port, then PORT, then 8080", func(t *testing.T) {
		tests := []struct {
			name      string
			flag, env string
			then      string
		}{
			{"flag wins", "9000", "9001", "9000"},
			{"PORT without flag", "", "9001", "9001"},
			{"default", "", "", "8080"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Given
				t.Setenv("PORT", tt.env)
				args := []string{"-in-memory", "-user-agent", "test test@example.com"}
				if tt.flag != "" {
					args = append(args, "-port", tt.flag)
				}

				// When
				cfg, err := parseConfig(args)

				// Then
				if err != nil || cfg.port != tt.then {
					t.Errorf("port = %q, %v; want %q", cfg.port, err, tt.then)
				}
			})
		}
	})
}
