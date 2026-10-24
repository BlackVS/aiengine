package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackVS/aiengine/pace"
)

// fakeEmbedServer answers /embeddings with vectors of the given width,
// ignoring or honouring the request's "dimensions" as configured.
func fakeEmbedServer(t *testing.T, width int, honourDim bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		n := width
		if honourDim && req.Dimensions > 0 {
			n = req.Dimensions
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := range req.Input {
			data = append(data, item{Index: i, Embedding: make([]float32, n)})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data":  data,
			"usage": map[string]int64{"total_tokens": 7},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedWidthGuard(t *testing.T) {
	// A provider that ignores "dimensions" hands back full-width vectors
	// that would be stored under the truncated key: Embed must refuse.
	srv := fakeEmbedServer(t, 3072, false)
	c := &Client{BaseURL: srv.URL, Dimensions: 768}
	if _, _, err := c.Embed(context.Background(), "m", []string{"x"}); err == nil ||
		!strings.Contains(err.Error(), "ignored dimensions") {
		t.Fatalf("width mismatch not refused: %v", err)
	}
}

func TestEmbedHonouredDimensions(t *testing.T) {
	srv := fakeEmbedServer(t, 3072, true)
	c := &Client{BaseURL: srv.URL, Dimensions: 768}
	vecs, u, err := c.Embed(context.Background(), "m", []string{"a", "b"})
	if err != nil || len(vecs) != 2 || len(vecs[0]) != 768 || u.InputTokens != 7 {
		t.Fatalf("honoured dims: vecs=%d u=%+v err=%v", len(vecs), u, err)
	}
}

// recorded is what the fake endpoint saw of one request.
type recorded struct {
	path, auth, ctype string
	body              map[string]any
}

// fakeServer serves every request with status/body/header and records it.
func fakeServer(t *testing.T, status int, body string, header map[string]string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type")}
		json.Unmarshal(raw, &rec.body)
		seen = append(seen, rec)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestCompleteHappyPath(t *testing.T) {
	srv, seen := fakeServer(t, 200,
		`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`, nil)
	c := &Client{BaseURL: srv.URL + "/", Token: "sk-test"} // a trailing slash is tolerated
	got, u, err := c.Complete(context.Background(), "gpt-4o", "hello")
	if err != nil || got != "ok" {
		t.Fatalf("Complete: %q err=%v", got, err)
	}
	if u.InputTokens != 12 || u.OutputTokens != 3 || u.CostUSD != 0 {
		t.Fatalf("usage: %+v", u)
	}
	r := (*seen)[0]
	if r.path != "/chat/completions" || r.auth != "Bearer sk-test" || r.ctype != "application/json" {
		t.Fatalf("request: %+v", r)
	}
	msgs, _ := r.body["messages"].([]any)
	if r.body["model"] != "gpt-4o" || len(msgs) != 1 || len(r.body) != 2 {
		t.Fatalf("payload shape: %+v", r.body)
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("message: %+v", m)
	}
}

func TestCompleteReadsProxyCost(t *testing.T) {
	srv, _ := fakeServer(t, 200,
		`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		map[string]string{"x-litellm-response-cost": "0.00042"})
	_, u, err := (&Client{BaseURL: srv.URL, Token: "k"}).Complete(context.Background(), "m", "p")
	if err != nil || u.CostUSD != 0.00042 {
		t.Fatalf("cost header: %+v err=%v", u, err)
	}
}

func TestCompleteErrorBody(t *testing.T) {
	srv, seen := fakeServer(t, 400,
		`{"error":{"message":"The model does-not-exist does not exist"},"usage":{"prompt_tokens":0}}`, nil)
	p := pace.New(pace.Options{Retries: 3})
	_, _, err := (&Client{BaseURL: srv.URL, Token: "k", Pacer: p}).Complete(context.Background(), "does-not-exist", "p")
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error body not surfaced: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("an ordinary error must not be retried: %d requests", len(*seen))
	}
}

func TestCompleteBadJSONAndNoChoices(t *testing.T) {
	srv, _ := fakeServer(t, 200, `not json`, nil)
	if _, _, err := (&Client{BaseURL: srv.URL}).Complete(context.Background(), "m", "p"); err == nil ||
		!strings.Contains(err.Error(), "bad completions response") {
		t.Fatalf("bad JSON: %v", err)
	}
	srv, _ = fakeServer(t, 200, `{"choices":[]}`, nil)
	if _, _, err := (&Client{BaseURL: srv.URL}).Complete(context.Background(), "m", "p"); err == nil ||
		!strings.Contains(err.Error(), "no choices") {
		t.Fatalf("no choices: %v", err)
	}
}

// instantClock makes pacer sleeps free while recording them.
type instantClock struct{ slept atomic.Int64 }

func (*instantClock) Now() time.Time { return time.Now() }
func (c *instantClock) Sleep(ctx context.Context, d time.Duration) error {
	c.slept.Add(int64(d))
	return ctx.Err()
}

// A rate-limit answer is retried with backoff through the pacer, widens
// its spacing, and a later success goes through.
func TestCompleteRetriesBlocks(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"rate limited"}}`)
		case 2: // a proxy wrapping an upstream block page in its JSON error
			io.WriteString(w, `{"error":{"message":"<html><title>Attention Required! | Cloudflare</title>"}}`)
		default:
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}
	}))
	defer srv.Close()
	clk := &instantClock{}
	p := pace.New(pace.Options{Retries: 3, Clock: clk})
	got, _, err := (&Client{BaseURL: srv.URL, Pacer: p}).Complete(context.Background(), "m", "p")
	if err != nil || got != "ok" || calls.Load() != 3 {
		t.Fatalf("retry: %q err=%v calls=%d", got, err, calls.Load())
	}
	if s := p.Status(); s.Blocks != 2 {
		t.Fatalf("blocks not counted: %+v", s)
	}
	if time.Duration(clk.slept.Load()) < 45*time.Second { // two backoffs: ~15s + ~30s
		t.Fatalf("backoff not taken: slept %s", time.Duration(clk.slept.Load()))
	}
}

func TestCompleteGivesUpAfterRetries(t *testing.T) {
	srv, seen := fakeServer(t, 503, `<html>down</html>`, nil)
	p := pace.New(pace.Options{Retries: 1, Clock: &instantClock{}})
	_, _, err := (&Client{BaseURL: srv.URL, Pacer: p}).Complete(context.Background(), "m", "p")
	if err == nil || !strings.Contains(err.Error(), "blocked upstream after 2 attempts") || len(*seen) != 2 {
		t.Fatalf("give up: err=%v requests=%d", err, len(*seen))
	}
	// Without a pacer there is no retry at all.
	srv, seen = fakeServer(t, 503, `<html>down</html>`, nil)
	if _, _, err := (&Client{BaseURL: srv.URL}).Complete(context.Background(), "m", "p"); err == nil || len(*seen) != 1 {
		t.Fatalf("unpaced: err=%v requests=%d", err, len(*seen))
	}
}

func TestEmbedCall(t *testing.T) {
	srv, seen := fakeServer(t, 200,
		`{"data":[{"index":1,"embedding":[0.5,0.25]},{"index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":9}}`, nil)
	c := &Client{BaseURL: srv.URL, Token: "sk-e"}
	vecs, u, err := c.Embed(context.Background(), "text-embedding-3-small", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if vecs[0][0] != 1 || vecs[1][0] != 0.5 {
		t.Fatalf("vectors not placed by index: %v", vecs)
	}
	if u.InputTokens != 9 { // prompt_tokens when total_tokens is absent
		t.Fatalf("usage: %+v", u)
	}
	r := (*seen)[0]
	if r.path != "/embeddings" || r.auth != "Bearer sk-e" || r.body["model"] != "text-embedding-3-small" {
		t.Fatalf("request: %+v", r)
	}
	if _, ok := r.body["dimensions"]; ok {
		t.Fatalf("dimensions sent without being set: %+v", r.body)
	}
}

func TestEmbedErrors(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"no such model"}}`:  "no such model",
		`{"data":[{"index":0,"embedding":[1]}]}`: "1 vectors for 2 inputs",
		`{"data":[{"index":0},{"index":5}]}`:     "out-of-range index 5",
		`{"data":`:                               "bad embeddings response",
	}
	for body, want := range cases {
		srv, _ := fakeServer(t, 200, body, nil)
		_, _, err := (&Client{BaseURL: srv.URL}).Embed(context.Background(), "m", []string{"a", "b"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v, want %q", body, err, want)
		}
	}
}

func TestDefaultBaseURL(t *testing.T) {
	var got string
	c := &Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"x"}}]}`)), Header: http.Header{}}, nil
	})}
	if _, _, err := c.Complete(context.Background(), "m", "p"); err != nil {
		t.Fatal(err)
	}
	if got != DefaultBaseURL+"/chat/completions" {
		t.Fatalf("default base URL: %s", got)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
