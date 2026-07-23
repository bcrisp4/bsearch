package evalharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer returns an httptest.Server that writes body verbatim as the SSE
// response, flushing after every write so the client observes it as a
// stream rather than one buffered read.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("ResponseWriter does not support flushing")
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintln(w, line)
			flusher.Flush()
		}
	}))
}

func TestSummarize_ConcatenatesDeltasAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"A "}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"B "}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"C"}}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	srv := sseServer(t, sse)
	defer srv.Close()

	c := &ChatClient{Endpoint: srv.URL, Model: "test-model"}
	text, metrics, err := c.Summarize(context.Background(), "some document")
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	if text != "A B C" {
		t.Errorf("text = %q, want %q", text, "A B C")
	}
	if metrics.CompletionTokens != 3 {
		t.Errorf("CompletionTokens = %d, want 3", metrics.CompletionTokens)
	}
	if metrics.PromptTokens != 10 {
		t.Errorf("PromptTokens = %d, want 10", metrics.PromptTokens)
	}
	if metrics.TokensPerSec <= 0 {
		t.Errorf("TokensPerSec = %v, want > 0", metrics.TokensPerSec)
	}
}

func TestSummarize_FallbackTokenCountWithoutUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"A "}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"B "}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"C"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	srv := sseServer(t, sse)
	defer srv.Close()

	c := &ChatClient{Endpoint: srv.URL, Model: "test-model"}
	text, metrics, err := c.Summarize(context.Background(), "some document")
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	if text != "A B C" {
		t.Errorf("text = %q, want %q", text, "A B C")
	}
	if metrics.CompletionTokens != 3 {
		t.Errorf("CompletionTokens = %d, want 3 (delta-count fallback)", metrics.CompletionTokens)
	}
	if metrics.PromptTokens != 0 {
		t.Errorf("PromptTokens = %d, want 0 (server omitted usage)", metrics.PromptTokens)
	}
}

func TestSummarize_HTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	c := &ChatClient{Endpoint: srv.URL, Model: "test-model"}
	_, _, err := c.Summarize(context.Background(), "some document")
	if err == nil {
		t.Fatal("Summarize() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not contain %q", err.Error(), "500")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not contain %q", err.Error(), "boom")
	}
}

func TestSummarize_SendsPromptAndModel(t *testing.T) {
	type chatRequest struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}

	var captured chatRequest
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &ChatClient{Endpoint: srv.URL + "/v1", Model: "test-model"}
	doc := "the document text"
	_, _, err := c.Summarize(context.Background(), doc)
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}

	if capturedPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want %q", capturedPath, "/v1/chat/completions")
	}
	if captured.Model != "test-model" {
		t.Errorf("model = %q, want %q", captured.Model, "test-model")
	}
	if !captured.Stream {
		t.Error("stream = false, want true")
	}
	if !captured.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage = false, want true")
	}
	if len(captured.Messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(captured.Messages))
	}
	content := captured.Messages[0].Content
	if !strings.Contains(content, SummarizePrompt) {
		t.Errorf("message content does not contain SummarizePrompt")
	}
	if !strings.Contains(content, doc) {
		t.Errorf("message content does not contain doc text")
	}
}
