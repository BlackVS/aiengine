// Package aiengine is model access for Go programs: the kind interface
// every backend implements, the usage it reports, and Probe, a one-call
// health check over any of them.
//
// The backends live in sub-packages. Package provider resolves which
// endpoint serves a model from a registry file the caller names; package
// openai is an OpenAI-compatible HTTP client (chat completions and
// embeddings); package cli runs a vendor's own headless CLI as the user
// installed and configured it; package pace spaces and retries calls.
//
// The module reads no environment variables and opens no vendor
// credentials file. Configuration arrives as parameters from the caller.
package aiengine

import (
	"context"
	"errors"
	"strings"
)

// Usage is what one call cost, as the backend reported it. InputTokens
// counts every input token the backend charged for, cached ones
// included; CostUSD is set only when the backend reports money (the
// claude CLI does, and so does a LiteLLM proxy in a response header).
type Usage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Completer is the kind interface: one plain text completion of prompt
// by model. Every backend implements it (openai.Client, cli.Runner).
// Usage is returned even with an error when the backend reported spend.
type Completer interface {
	Complete(ctx context.Context, model, prompt string) (string, Usage, error)
}

// Versioner is implemented by backends that can name their own version
// (a vendor CLI's --version output). HTTP endpoints do not.
type Versioner interface {
	Version(ctx context.Context) (string, error)
}

// ErrLoginExpired marks an error where the backend reported that its own
// login is missing or no longer valid: the user has to sign in again in
// the vendor's tool. Test for it with errors.Is.
var ErrLoginExpired = errors.New("login expired")

// AuthState is a call's authentication verdict.
type AuthState string

const (
	AuthOK           AuthState = "ok"
	AuthLoginExpired AuthState = "login_expired"
	AuthError        AuthState = "error"
)

// AuthOf classifies a call's error: nil is AuthOK, an error wrapping
// ErrLoginExpired is AuthLoginExpired, anything else is AuthError.
func AuthOf(err error) AuthState {
	switch {
	case err == nil:
		return AuthOK
	case errors.Is(err, ErrLoginExpired):
		return AuthLoginExpired
	default:
		return AuthError
	}
}

// ProbePrompt is the fixed prompt Probe sends.
const ProbePrompt = "Reply with the single word: ok"

// ProbeResult is what one Probe observed.
type ProbeResult struct {
	Answer  string    // the model's reply, trimmed
	Version string    // the backend's own version, where it reports one
	Auth    AuthState // ok, login_expired or error
	Usage   Usage
	Err     error // the completion's error; nil when Auth is AuthOK
}

// Probe runs ProbePrompt once through c and reports the answer, the
// backend's version when c is a Versioner, and the auth state. A failed
// version query leaves Version empty and does not fail the probe: the
// completion is the health check.
func Probe(ctx context.Context, c Completer, model string) ProbeResult {
	var r ProbeResult
	if v, ok := c.(Versioner); ok {
		if ver, err := v.Version(ctx); err == nil {
			r.Version = ver
		}
	}
	answer, u, err := c.Complete(ctx, model, ProbePrompt)
	r.Answer, r.Usage, r.Err, r.Auth = strings.TrimSpace(answer), u, err, AuthOf(err)
	return r
}
