package openai

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// StatusError is a non-2xx response from the inference endpoint. Message
// carries a truncated response body for diagnostics — inference servers
// put the useful detail there.
type StatusError struct {
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("inference endpoint returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("inference endpoint returned HTTP %d: %s", e.StatusCode, e.Message)
}

// Transient reports whether err is worth retrying: connection/timeout
// failures and HTTP 408/429/5xx. Context cancellation is not transient —
// the caller chose to stop. Deadline expiry IS transient: it means the
// service was slow (the adapter's own http.Client timeout surfaces as
// context.DeadlineExceeded too), and a retry with a fresh deadline may
// succeed. Permanent errors (4xx content errors, malformed responses)
// burn the document's attempts; transient ones are the scheduler's
// queue-and-retry territory (DESIGN.md: response classification).
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		code := statusErr.StatusCode
		return code == 408 || code == 429 || code >= 500
	}
	// No status: the request never got an HTTP response — refused
	// connection, reset, DNS failure, client timeout. All retryable.
	var netErr net.Error
	return errors.As(err, &netErr)
}
