package aiengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type fakeCompleter struct {
	answer string
	err    error
	prompt string
}

func (f *fakeCompleter) Complete(_ context.Context, _, prompt string) (string, Usage, error) {
	f.prompt = prompt
	return f.answer, Usage{InputTokens: 4, OutputTokens: 1}, f.err
}

type fakeVersioned struct {
	fakeCompleter
	version string
	verErr  error
}

func (f *fakeVersioned) Version(context.Context) (string, error) { return f.version, f.verErr }

func TestProbe(t *testing.T) {
	c := &fakeCompleter{answer: " ok\n"}
	r := Probe(context.Background(), c, "m")
	if r.Answer != "ok" || r.Auth != AuthOK || r.Err != nil || r.Version != "" || r.Usage.InputTokens != 4 {
		t.Fatalf("ok probe: %+v", r)
	}
	if c.prompt != ProbePrompt {
		t.Fatalf("probe prompt: %q", c.prompt)
	}

	v := &fakeVersioned{fakeCompleter: fakeCompleter{answer: "ok"}, version: "1.2.3"}
	if r := Probe(context.Background(), v, "m"); r.Version != "1.2.3" || r.Auth != AuthOK {
		t.Fatalf("versioned probe: %+v", r)
	}
	// A failed version query does not fail the probe.
	v.verErr = errors.New("no --version")
	if r := Probe(context.Background(), v, "m"); r.Version != "" || r.Auth != AuthOK {
		t.Fatalf("version failure: %+v", r)
	}
}

func TestProbeAuthStates(t *testing.T) {
	expired := &fakeCompleter{err: fmt.Errorf("claude CLI: %w", ErrLoginExpired)}
	if r := Probe(context.Background(), expired, "m"); r.Auth != AuthLoginExpired || r.Err == nil {
		t.Fatalf("expired: %+v", r)
	}
	failed := &fakeCompleter{err: errors.New("HTTP 500")}
	if r := Probe(context.Background(), failed, "m"); r.Auth != AuthError || r.Err == nil {
		t.Fatalf("failed: %+v", r)
	}
}
