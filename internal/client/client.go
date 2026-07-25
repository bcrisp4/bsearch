// Package client talks to the daemon over its unix socket. It is deliberately
// thin: request validation, index compatibility, and every error message live
// on the server, so the CLI and any future client render the same words for
// the same failure (ADR 0009, ADR 0010).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/bcrisp4/bsearch/internal/search"
)

const (
	// timeout is the client's ceiling. It sits above the server's write
	// timeout so a slow request produces the daemon's own error — with a
	// message that explains it — rather than the client giving up first.
	timeout = 60 * time.Second

	// maxResponseBytes caps a response body. A search response is bounded
	// by the document limit; this only exists so a wedged server can't make
	// the client allocate without end.
	maxResponseBytes = 8 << 20
)

// Client is a connection to one daemon socket.
type Client struct {
	socket string
	http   *http.Client
}

// New builds a client for the daemon listening on socketPath.
func New(socketPath string) *Client {
	return &Client{
		socket: socketPath,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					// The URL's host is ignored: every request goes to the
					// one socket this client was built for.
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Error is a failure the daemon reported, carrying the wire contract's code
// so callers can branch without matching prose.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Search runs a query and returns the raw response body.
//
// Raw, not decoded: the CLI's --json output is a promise to its callers, and
// copying the daemon's bytes through keeps fields this build has never heard
// of — M4's score and summary — flowing to consumers that do understand them.
// Callers that need the values decode it themselves.
func (c *Client) Search(ctx context.Context, req search.Request) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/search", body)
}

// Status fetches the daemon's status document, raw for the same reason.
func (c *Client) Status(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/status", nil)
}

func (c *Client) do(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	// The host is a placeholder; the transport dials the socket regardless.
	req, err := http.NewRequestWithContext(ctx, method, "http://bsearch"+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.dialError(err)
	}
	defer resp.Body.Close() //nolint:errcheck // response fully read below

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response from the daemon: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeError(resp.StatusCode, data)
	}
	return data, nil
}

// dialError turns a transport failure into advice. The two common cases —
// no daemon at all, and a socket left behind by one that died — look almost
// identical from here and need different fixes, so they are told apart.
func (c *Client) dialError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("the bsearch daemon is not running (no socket at %s) — start it with 'bsearch serve'", c.socket)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("the socket at %s exists but nothing is listening — the daemon died; start it again with 'bsearch serve'", c.socket)
	case errors.Is(err, syscall.EACCES), errors.Is(err, os.ErrPermission):
		return fmt.Errorf("permission denied on %s — the socket belongs to another user", c.socket)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the bsearch daemon did not answer in time")
	}
	return fmt.Errorf("cannot reach the bsearch daemon on %s: %w", c.socket, err)
}

// decodeError reads the daemon's error envelope. A response that isn't one —
// a proxy, a panic that escaped, a future version — degrades to naming the
// status rather than dumping an unknown body at the user.
func decodeError(status int, body []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Message == "" {
		return &Error{
			Status:  status,
			Message: fmt.Sprintf("the bsearch daemon returned HTTP %d", status),
		}
	}
	return &Error{Status: status, Code: env.Error.Code, Message: env.Error.Message}
}
