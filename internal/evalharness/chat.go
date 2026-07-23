package evalharness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SummarizePrompt is the fixed prompt of the M2 summarizer bench —
// approximates the future pyramid gist level. Never varied per model.
const SummarizePrompt = "Summarize this document in 3-4 plain-English sentences. State what kind of document it is, who it is from, the key amounts and dates, and what it is about."

// maxErrorBodyBytes caps how much of a non-2xx response body is included in
// the returned error — enough to diagnose a bad request without unbounded
// growth from a misbehaving server.
const maxErrorBodyBytes = 512

// ChatMetrics reports one summarize call.
type ChatMetrics struct {
	PromptTokens     int     `json:"prompt_tokens"`     // 0 when server omits usage
	CompletionTokens int     `json:"completion_tokens"` // fallback: SSE delta count
	WallSeconds      float64 `json:"wall_seconds"`
	TokensPerSec     float64 `json:"tokens_per_sec"` // completion tokens / seconds since first delta
}

// ChatClient streams chat completions from an OpenAI-compatible endpoint.
//
// Never logs document content: request payloads (which embed the corpus
// document) are not written to any log, only used to build the HTTP
// request. Error paths may include HTTP status and a truncated response
// body, never the request.
type ChatClient struct {
	Endpoint string // e.g. http://localhost:1234/v1
	Model    string
	HTTP     *http.Client // nil → 5-minute-timeout default client
}

// chatRequest is the wire shape of an OpenAI-compatible chat completion
// request, streaming with usage reporting enabled.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions chatStreamOpts `json:"stream_options"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatChunk is one decoded SSE `data: ` payload. Usage is present only on
// the final chunk, at which point Choices may be empty.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Summarize sends SummarizePrompt + doc as one user message with
// stream=true and stream_options.include_usage, returning the full response
// text and throughput metrics.
func (c *ChatClient) Summarize(ctx context.Context, doc string) (string, ChatMetrics, error) {
	reqBody := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "user", Content: SummarizePrompt + "\n\n" + doc},
		},
		Stream:        true,
		StreamOptions: chatStreamOpts{IncludeUsage: true},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", ChatMetrics{}, fmt.Errorf("summarize: encode request: %w", err)
	}

	url := strings.TrimSuffix(c.Endpoint, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", ChatMetrics{}, fmt.Errorf("summarize: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", ChatMetrics{}, fmt.Errorf("summarize: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return "", ChatMetrics{}, fmt.Errorf("summarize: status %d: %s", resp.StatusCode, body)
	}

	var text strings.Builder
	var metrics ChatMetrics
	var firstDelta, lastEvent time.Time
	deltaCount := 0

	scanner := bufio.NewScanner(resp.Body)
	// SSE chunks (a full summary's worth of content deltas) can exceed the
	// scanner's default 64KiB line limit; give it headroom.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			// Blank lines and `: comment` lines are part of the SSE
			// framing, not data — skip rather than error.
			continue
		}
		lastEvent = time.Now()
		if data == "[DONE]" {
			break
		}

		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", ChatMetrics{}, fmt.Errorf("summarize: malformed SSE chunk: %w", err)
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			if firstDelta.IsZero() {
				firstDelta = time.Now()
			}
			text.WriteString(choice.Delta.Content)
			deltaCount++
		}

		if chunk.Usage != nil {
			metrics.PromptTokens = chunk.Usage.PromptTokens
			metrics.CompletionTokens = chunk.Usage.CompletionTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return "", ChatMetrics{}, fmt.Errorf("summarize: read stream: %w", err)
	}

	metrics.WallSeconds = time.Since(start).Seconds()
	if metrics.CompletionTokens == 0 {
		metrics.CompletionTokens = deltaCount
	}

	span := 0.0
	if !firstDelta.IsZero() && !lastEvent.IsZero() {
		span = lastEvent.Sub(firstDelta).Seconds()
	}
	if span == 0 {
		span = metrics.WallSeconds
	}
	if span > 0 {
		metrics.TokensPerSec = float64(metrics.CompletionTokens) / span
	}

	return text.String(), metrics, nil
}
