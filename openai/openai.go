// Package openai is a client for OpenAI-compatible HTTP endpoints: chat
// completions and embeddings, with the caller's token, paced and retried
// through an optional pace.Pacer. Point BaseURL at OpenAI itself or at a
// compatible proxy (LiteLLM, vLLM, Ollama, a vendor gateway).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BlackVS/aiengine"
	"github.com/BlackVS/aiengine/pace"
)

// DefaultBaseURL is where a client points when it names no URL of its own.
const DefaultBaseURL = "https://api.openai.com/v1"

const (
	// DefaultTimeout bounds one completion; DefaultEmbedTimeout one
	// embeddings request.
	DefaultTimeout      = 5 * time.Minute
	DefaultEmbedTimeout = 60 * time.Second

	maxBody = 8 << 20
)

// Client calls one OpenAI-compatible endpoint. The zero value plus a
// Token talks to DefaultBaseURL, unpaced.
type Client struct {
	BaseURL string // "" = DefaultBaseURL
	Token   string // sent as "Authorization: Bearer <Token>"
	// Dimensions truncates embeddings (OpenAI "dimensions"; honoured by
	// text-embedding-3-* and Google's compatible endpoint). 0 = the
	// provider's default width. A provider that ignores it is refused,
	// because full-width vectors stored under a truncated name would mix
	// two incompatible spaces.
	Dimensions   int
	Timeout      time.Duration     // per completion; 0 = DefaultTimeout
	EmbedTimeout time.Duration     // per embeddings request; 0 = DefaultEmbedTimeout
	Pacer        *pace.Pacer       // nil = unpaced, no retries
	Transport    http.RoundTripper // nil = http.DefaultTransport
}

// errorBody is the error envelope OpenAI-compatible endpoints share.
type errorBody struct {
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// reply is one finished HTTP exchange that was not a transient block.
type reply struct {
	status int
	raw    []byte
	header http.Header
}

// post sends body to path, paced, retrying transient upstream blocks with
// backoff: a rate-limit or server status, a body that is not JSON and
// looks like a block page, or a proxy's error envelope wrapping one. A
// JSON body is never searched as a whole, because a model's answer may
// mention Cloudflare. Every other response is returned as is for the
// caller to decode.
func (c *Client) post(ctx context.Context, path string, body []byte, timeout time.Duration, what string) (reply, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	hc := &http.Client{Timeout: timeout, Transport: c.Transport}
	for attempt := 0; ; attempt++ {
		if err := c.Pacer.Wait(ctx); err != nil {
			return reply{}, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+path, bytes.NewReader(body))
		if err != nil {
			return reply{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := hc.Do(req)
		if err != nil {
			return reply{}, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		var eb errorBody
		_ = json.Unmarshal(raw, &eb)
		reason := ""
		switch {
		case resp.StatusCode == 429 || resp.StatusCode >= 500,
			!json.Valid(raw) && pace.BlockedMessage(string(raw)):
			reason = fmt.Sprintf("%s HTTP %d", what, resp.StatusCode)
		case eb.Error != nil && pace.BlockedMessage(eb.Error.Message):
			reason = what + " proxy error: " + pace.Clip(eb.Error.Message, 120)
		}
		if reason == "" {
			return reply{status: resp.StatusCode, raw: raw, header: resp.Header}, nil
		}
		c.Pacer.Penalize(reason)
		if attempt >= c.Pacer.Retries() {
			return reply{status: resp.StatusCode, raw: raw, header: resp.Header},
				fmt.Errorf("%s blocked upstream after %d attempts (HTTP %d): %s",
					what, attempt+1, resp.StatusCode, pace.Clip(string(raw), 200))
		}
		d := pace.RetryDelay(attempt)
		c.Pacer.Logf("retrying %s in %s (attempt %d/%d)", what, d.Round(time.Second), attempt+1, c.Pacer.Retries())
		if err := c.Pacer.Sleep(ctx, d); err != nil {
			return reply{}, err
		}
	}
}

// Complete runs one plain text chat completion of prompt by model and
// returns the first choice's content with the reported token usage. A
// LiteLLM proxy's x-litellm-response-cost header fills Usage.CostUSD.
func (c *Client) Complete(ctx context.Context, model, prompt string) (string, aiengine.Usage, error) {
	var u aiengine.Usage
	// No temperature field: GPT-5-family models reject anything but the
	// default.
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	r, err := c.post(ctx, "/chat/completions", body, timeout, "completions")
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		errorBody
	}
	jsonErr := json.Unmarshal(r.raw, &out)
	u = aiengine.Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	if cost, perr := strconv.ParseFloat(r.header.Get("x-litellm-response-cost"), 64); perr == nil && cost > 0 {
		u.CostUSD = cost
	}
	if err != nil {
		return "", u, err
	}
	if jsonErr != nil {
		return "", u, fmt.Errorf("bad completions response (HTTP %d): %s", r.status, pace.Clip(string(r.raw), 200))
	}
	// A parsed, unblocked answer is a healthy upstream, even when it
	// carries the caller's own error (a bad model name, a 400).
	c.Pacer.Recover()
	if out.Error != nil {
		return "", u, fmt.Errorf("completions error (HTTP %d): %s", r.status, pace.Clip(out.Error.Message, 200))
	}
	if len(out.Choices) == 0 {
		return "", u, fmt.Errorf("completions returned no choices (HTTP %d)", r.status)
	}
	return out.Choices[0].Message.Content, u, nil
}

// Embed returns one vector per input text, in order, and the request's
// token usage in Usage.InputTokens (total_tokens, or prompt_tokens when
// the endpoint reports only that).
func (c *Client) Embed(ctx context.Context, model string, texts []string) ([][]float32, aiengine.Usage, error) {
	payload := map[string]any{"model": model, "input": texts}
	if c.Dimensions > 0 {
		payload["dimensions"] = c.Dimensions
	}
	body, _ := json.Marshal(payload)
	timeout := c.EmbedTimeout
	if timeout <= 0 {
		timeout = DefaultEmbedTimeout
	}
	r, err := c.post(ctx, "/embeddings", body, timeout, "embeddings")
	if err != nil {
		return nil, aiengine.Usage{}, err
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
		errorBody
	}
	if json.Unmarshal(r.raw, &out) != nil {
		return nil, aiengine.Usage{}, fmt.Errorf("bad embeddings response (HTTP %d): %s", r.status, pace.Clip(string(r.raw), 200))
	}
	c.Pacer.Recover()
	u := aiengine.Usage{InputTokens: out.Usage.TotalTokens}
	if u.InputTokens == 0 {
		u.InputTokens = out.Usage.PromptTokens
	}
	if out.Error != nil {
		return nil, u, fmt.Errorf("embeddings error (HTTP %d): %s", r.status, pace.Clip(out.Error.Message, 200))
	}
	if len(out.Data) != len(texts) {
		return nil, u, fmt.Errorf("embeddings returned %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, u, fmt.Errorf("embeddings returned out-of-range index %d", d.Index)
		}
		if c.Dimensions > 0 && len(d.Embedding) != c.Dimensions {
			return nil, u, fmt.Errorf("provider ignored dimensions=%d and returned %d (model %q)",
				c.Dimensions, len(d.Embedding), model)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, u, nil
}
