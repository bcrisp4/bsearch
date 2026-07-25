package search

import "errors"

// Sentinels classifying every way a search can fail. Transports map them to
// their own vocabulary — the HTTP server to status codes and error codes (ADR
// 0009), the CLI to an exit status — so no transport has to inspect messages.
//
// Errors wrapping these carry user-facing prose: the daemon's message is
// rendered verbatim by the CLI, so it must read as advice, not as a code.
var (
	// ErrInvalidRequest means the caller's request is malformed: an empty
	// query, a limit out of range, an unsupported mode, a query longer than
	// the model's input ceiling. Retrying unchanged cannot help.
	ErrInvalidRequest = errors.New("invalid search request")

	// ErrNotIndexed means there is nothing to search yet — no vector table,
	// or the generation in use was retired mid-query by an indexer running
	// elsewhere. The remedy is to index, or to wait, never to change the
	// request.
	ErrNotIndexed = errors.New("index not ready")

	// ErrIndexMismatch means the configured embedding identity differs from
	// the one the index was built with (model, prefix templates, or
	// dimensions). Searching anyway would compare vectors from different
	// spaces, so the query is refused rather than answered wrongly.
	ErrIndexMismatch = errors.New("index and configuration disagree")

	// ErrEmbedder means the query could not be embedded. It deliberately
	// does not distinguish "server unreachable" from "malformed response":
	// the inference adapter exposes no sentinels, and inventing them there
	// is a separate concern (ADR 0009). Deadlines are excluded — a timeout
	// stays a context error, because it points at a different problem.
	ErrEmbedder = errors.New("could not embed the query")
)
