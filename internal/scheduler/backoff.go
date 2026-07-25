package scheduler

import "time"

const (
	// defaultBackoffBase is the first retry window. Long enough that a
	// momentary blip has passed, short enough that a document which trips one
	// is still indexed within the freshness SLO.
	defaultBackoffBase = 30 * time.Second
	// defaultBackoffCap bounds the window. It can be this short because
	// backoff only ever applies to a document that failed while the embedding
	// endpoint was verifiably healthy — a genuine outage is handled by the
	// health gate, which defers the whole batch and burns no attempts at all.
	defaultBackoffCap = 15 * time.Minute
	// defaultMaxAttempts is how many transient failures a document absorbs
	// before it is called failed. Five spans roughly half an hour of retrying
	// under the window above; past that the evidence says the document, not
	// the weather.
	defaultMaxAttempts = 5
)

// fullJitter returns the backoff window for a given attempt:
//
//	random(0, min(ceiling, base · 2^(attempt-1)))
//
// AWS's "full jitter" — of the published variants it does the least total
// work and produces the lowest load on the thing that is already struggling,
// at the cost of slightly longer completion. That trade is the right way
// round here: the endpoint being retried is a local inference server sharing
// a laptop with everything else, so being gentle with it matters more than
// finishing a retry a few seconds sooner.
//
// The randomness is what stops a batch of documents that failed together from
// coming back together. Without it, one blip converts a batch into a
// synchronised herd that re-creates the blip.
//
// rnd returns a value in [0, n). attempt is 1-based; anything lower is
// treated as the first attempt.
func fullJitter(attempt int, base, ceiling time.Duration, rnd func(int64) int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	window := base
	// Doubling in a loop rather than shifting: the shift would overflow into
	// a negative duration long before a realistic attempt count, and a
	// negative window is a retry scheduled in the past — a hot loop.
	for i := 1; i < attempt && window < ceiling; i++ {
		window *= 2
	}
	if window > ceiling {
		window = ceiling
	}
	if window <= 0 {
		return 0
	}
	return time.Duration(rnd(int64(window)))
}
