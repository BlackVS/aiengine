package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BlackVS/aiengine"
	"github.com/BlackVS/aiengine/pace"
)

// Claude is the Vendor for Anthropic's Claude Code CLI, run headless as
// `claude -p --model <model> --output-format json` with the prompt on
// stdin.
type Claude struct{}

func (Claude) Name() string         { return "claude" }
func (Claude) DefaultBin() string   { return "claude" }
func (Claude) DefaultModel() string { return "haiku" }

func (Claude) Args(model string) []string {
	return []string{"-p", "--model", model, "--output-format", "json"}
}

func (Claude) VersionArgs() []string { return []string{"--version"} }

// Parse decodes the headless CLI's JSON result. The top-level usage's
// input_tokens is only the UNCACHED slice of the final API call; in a
// `claude -p` run nearly the whole prompt rides prompt caching, so the
// real input is uncached + cache-creation + cache-read. Cache-read
// tokens are far cheaper, so the count slightly OVERSTATES relative
// cost (the safe direction for a budget); total_cost_usd stays the
// money truth.
func (Claude) Parse(out []byte) (string, aiengine.Usage, error) {
	var w struct {
		Result  string  `json:"result"`
		IsError bool    `json:"is_error"`
		CostUSD float64 `json:"total_cost_usd"`
		Usage   struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &w); err != nil {
		return "", aiengine.Usage{}, fmt.Errorf("claude: %w: %w", ErrBadOutput, err)
	}
	u := aiengine.Usage{
		InputTokens:  w.Usage.InputTokens + w.Usage.CacheCreationTokens + w.Usage.CacheReadTokens,
		OutputTokens: w.Usage.OutputTokens,
		CostUSD:      w.CostUSD,
	}
	if w.IsError {
		return "", u, fmt.Errorf("completion turn errored: %s", pace.Clip(w.Result, 200))
	}
	return w.Result, u, nil
}

// LoginExpired recognises the CLI's own sign-in failures. Signed out,
// Claude Code 2.1 prints its JSON result with is_error and the text
// "Not logged in · Please run /login" and exits 1; a login that expired
// and cannot be refreshed fails with "Login expired · Please run /login"
// (the CLI's authentication documentation); a revoked one carries the
// API's authentication_error inside an "API Error: 401" result.
func (Claude) LoginExpired(output string) bool {
	m := strings.ToLower(output)
	for _, s := range []string{
		"please run /login",
		"not logged in",
		"login expired",
		"oauth token has expired",
		"oauth token revoked",
		"authentication_error",
		"invalid api key",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// Blocked reports a rate-limit shaped failure. The CLI does not emit
// Cloudflare HTML: it says "Claude AI usage limit reached|<ts>" on
// stderr or carries "API Error: 429 ..." inside its stdout JSON, so the
// generic detector alone would never fire here.
func (Claude) Blocked(output string) bool {
	if pace.BlockedMessage(output) {
		return true
	}
	m := strings.ToLower(output)
	for _, s := range []string{"usage limit", "rate limit", "429", "529", "overloaded"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
