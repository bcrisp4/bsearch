package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/bcrisp4/bsearch/internal/search"
)

// Error codes. Stable machine tokens: clients switch on them, so they are
// part of the API contract (ADR 0009) and outlive any rewording of messages.
const (
	codeBadRequest      = "bad_request"
	codeNotFound        = "not_found"
	codeMethodNotAllwed = "method_not_allowed"
	codeIndexMismatch   = "index_mismatch"
	codeTooLarge        = "payload_too_large"
	codeInternal        = "internal"
	codeEmbedder        = "embedder_unavailable"
	codeNotIndexed      = "not_indexed"
	codeBusy            = "busy"
	codeTimeout         = "timeout"
)

// genericInternalMessage is what a client sees for an unclassified failure.
// The real error goes to the log: it can carry paths, driver internals, or
// document detail that has no business crossing the socket.
const genericInternalMessage = "the daemon hit an unexpected error — check its log"

// errorBody is the envelope every non-2xx response carries, including the
// ones net/http would otherwise answer in plain text.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError sends the envelope. Encoding failures are logged rather than
// returned: the status line is already written, so there is nothing left to
// tell the client.
func writeError(w http.ResponseWriter, log *slog.Logger, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: message}}); err != nil {
		log.Error("write error response", "error", err)
	}
}

// writeJSON sends a 200 with a JSON body.
func writeJSON(w http.ResponseWriter, log *slog.Logger, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Too late for a status line; the client sees a truncated body.
		log.Error("write response", "error", err)
	}
}

// classify maps a search failure onto the wire contract. Order matters: a
// deadline is checked first because a slow embedder would otherwise be
// reported as an unreachable one, sending the user somewhere different from
// where the problem is.
func classify(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, codeTimeout, "the search took too long — the inference server may be slow or loading a model"
	case errors.Is(err, search.ErrInvalidRequest):
		return http.StatusBadRequest, codeBadRequest, search.Message(err)
	case errors.Is(err, search.ErrNotIndexed):
		return http.StatusServiceUnavailable, codeNotIndexed, search.Message(err)
	case errors.Is(err, search.ErrIndexMismatch):
		return http.StatusConflict, codeIndexMismatch, search.Message(err)
	case errors.Is(err, search.ErrEmbedder):
		return http.StatusBadGateway, codeEmbedder, search.Message(err)
	default:
		return http.StatusInternalServerError, codeInternal, genericInternalMessage
	}
}
