package scheduler

import (
	"math/rand/v2"
	"testing"
	"time"
)

// maxRand pins the jitter to the top of its window, so a test can assert the
// window itself rather than a range.
func maxRand(n int64) int64 { return n - 1 }

func TestFullJitterWindowDoublesAndCaps(t *testing.T) {
	base, ceiling := 30*time.Second, 15*time.Minute

	tests := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{"first retry is one base interval", 1, 30 * time.Second},
		{"second doubles", 2, time.Minute},
		{"third doubles again", 3, 2 * time.Minute},
		{"fourth", 4, 4 * time.Minute},
		{"fifth", 5, 8 * time.Minute},
		{"sixth would exceed the cap", 6, 15 * time.Minute},
		{"far beyond the cap stays capped", 40, 15 * time.Minute},
		// A caller that miscounts must not produce a longer wait than the
		// first retry, and must never produce a negative one.
		{"attempt zero is treated as the first", 0, 30 * time.Second},
		{"a negative attempt is treated as the first", -3, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fullJitter(tt.attempt, base, ceiling, maxRand)
			if got != tt.want-1 {
				t.Errorf("fullJitter(%d) = %v, want just under %v", tt.attempt, got, tt.want)
			}
		})
	}
}

// The whole point of jitter is that a batch of documents which failed
// together does not come back together and re-create the failure.
func TestFullJitterSpreadsRetries(t *testing.T) {
	base, ceiling := 30*time.Second, 15*time.Minute
	rnd := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test source

	seen := map[time.Duration]int{}
	for range 200 {
		d := fullJitter(4, base, ceiling, rnd.Int64N)
		if d < 0 {
			t.Fatalf("fullJitter returned a negative duration %v — a retry in the past is a hot loop", d)
		}
		if d >= 4*time.Minute {
			t.Fatalf("fullJitter(4) = %v, want it inside the 4m window", d)
		}
		seen[d]++
	}
	// Full jitter samples the whole window, so 200 draws landing on a handful
	// of values would mean the randomness is not reaching the result.
	if len(seen) < 100 {
		t.Errorf("200 draws produced %d distinct waits, want them spread across the window", len(seen))
	}
}

// Full jitter's lower bound is zero by construction. Confirming it here is
// what makes it a decision rather than a surprise: an immediate retry is
// harmless because a document already worked in this drain is not re-claimed
// until the next cycle.
func TestFullJitterCanReturnZero(t *testing.T) {
	got := fullJitter(3, 30*time.Second, 15*time.Minute, func(int64) int64 { return 0 })
	if got != 0 {
		t.Errorf("fullJitter with a zero draw = %v, want 0", got)
	}
}

// A zero or negative base would otherwise ask rnd for a value in [0, 0),
// which panics in math/rand/v2.
func TestFullJitterHandlesADegenerateWindow(t *testing.T) {
	for _, base := range []time.Duration{0, -time.Second} {
		got := fullJitter(2, base, 15*time.Minute, func(n int64) int64 {
			t.Errorf("rnd called with n = %d for a non-positive window", n)
			return 0
		})
		if got != 0 {
			t.Errorf("fullJitter with base %v = %v, want 0", base, got)
		}
	}
}

// The defaults are an explicit limit, not an accident: five attempts across
// the capped window is roughly half an hour of retrying before a document is
// called failed (ADR 0011).
func TestDefaultBackoffBudgetIsBounded(t *testing.T) {
	var worst time.Duration
	for attempt := 1; attempt < defaultMaxAttempts; attempt++ {
		worst += fullJitter(attempt, defaultBackoffBase, defaultBackoffCap, maxRand)
	}
	if worst > time.Hour {
		t.Errorf("worst-case retry budget is %v, want it inside an hour", worst)
	}
	if worst < 5*time.Minute {
		t.Errorf("worst-case retry budget is %v, want long enough to outlast a restart", worst)
	}
}
