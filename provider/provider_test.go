package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests are aimem's internal/provider tests with its two
// constants turned into parameters: the state root became the registry
// path, and the AIMEM_OPENAI_* environment became a Fallback value.

func regPath(t *testing.T) string { return filepath.Join(t.TempDir(), "providers.json") }

var envFallback = Fallback{BaseURL: "https://env.example/v1", Token: "envkey", Source: "AIMEM_OPENAI_API_KEY"}

func TestResolvePrecedence(t *testing.T) {
	path := regPath(t)
	fb := envFallback

	// No registry: fallback.
	ep, ok := Resolve(path, "m1", fb)
	if !ok || ep.Token != "envkey" || ep.BaseURL != "https://env.example/v1" || ep.Kind != "openai" || !ep.FromFallback {
		t.Fatalf("fallback: %+v ok=%v", ep, ok)
	}

	// Registry binding wins over the fallback.
	r := Load(path)
	r.Providers["g"] = Provider{Kind: "openai", BaseURL: "https://g.example/v1", Token: "gkey"}
	r.Providers["cl"] = Provider{Kind: "claude"}
	r.Bind("m1", "g", "")
	r.Bind("m2", "cl", "")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("registry mode: %v err=%v", fi.Mode(), err)
	}
	if ep, ok = Resolve(path, "m1", fb); !ok || ep.Token != "gkey" || ep.BaseURL != "https://g.example/v1" || ep.FromFallback {
		t.Fatalf("registry binding: %+v ok=%v", ep, ok)
	}
	if ep, ok = Resolve(path, "m2", fb); !ok || ep.Kind != "claude" || ep.Token != "" {
		t.Fatalf("claude kind: %+v ok=%v", ep, ok)
	}
	// Unbound model still falls back.
	if ep, ok = Resolve(path, "other", fb); !ok || ep.Token != "envkey" || !ep.FromFallback {
		t.Fatalf("unbound fallback: %+v ok=%v", ep, ok)
	}
	// A BOUND model whose provider cannot serve it must NOT fall back:
	// the binding is explicit, and serving it from the fallback hid a
	// misconfigured provider behind another vendor's errors.
	r.Providers["hollow"] = Provider{Kind: "openai"}
	r.Bind("m3", "hollow", "")
	r.Bind("m4", "vanished", "")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	if ep, ok = Resolve(path, "m3", fb); ok {
		t.Fatalf("hollow binding must not resolve (got %+v)", ep)
	}
	if ep, ok = Resolve(path, "m4", fb); ok {
		t.Fatalf("binding to a missing provider must not resolve (got %+v)", ep)
	}
}

// The contract every caller relies on: Explain is empty exactly when
// Resolve succeeds, over the whole case set — one classifier, no drift.
func TestExplainMatchesResolve(t *testing.T) {
	path := regPath(t)
	fb := Fallback{Token: "envkey", Source: "AIMEM_OPENAI_API_KEY"}
	r := Load(path)
	r.Providers["g"] = Provider{Kind: "openai", Token: "gkey"}
	r.Providers["cl"] = Provider{Kind: "claude"}
	r.Providers["hollow"] = Provider{Kind: "openai"}
	r.Bind("healthy", "g", "")
	r.Bind("viacli", "cl", "")
	r.Bind("tokenless", "hollow", "")
	r.Bind("orphan", "vanished", "")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{ // model -> expected reason fragment ("" = resolves)
		"healthy":   "",
		"viacli":    "",
		"tokenless": `provider "hollow"`,
		"orphan":    "no longer exists",
		"unbound":   "", // the fallback serves it
	}
	for m, want := range cases {
		_, ok := Resolve(path, m, fb)
		why := Explain(path, m, "", fb)
		if (why == "") != ok {
			t.Errorf("%s: Resolve ok=%v but Explain=%q", m, ok, why)
		}
		if want != "" && !strings.Contains(why, want) {
			t.Errorf("%s: reason %q lacks %q", m, why, want)
		}
	}
	if !strings.Contains(Explain(path, "tokenless", "", fb), "no token") {
		t.Errorf("tokenless reason must say so: %q", Explain(path, "tokenless", "", fb))
	}
	// The kind requirement: a claude binding is healthy in general but
	// not where an OpenAI-compatible endpoint is required, and the
	// reason must say that, not stay empty.
	if ep, why := Lookup(path, "viacli", "openai", fb); why == "" || !strings.Contains(why, "claude") {
		t.Errorf("claude binding under wantKind=openai: ep=%+v why=%q", ep, why)
	}
	if _, why := Lookup(path, "healthy", "openai", fb); why != "" {
		t.Errorf("openai binding under wantKind=openai: %q", why)
	}
	// Unbound without a fallback token: both halves flip together.
	fb.Token = ""
	if _, ok := Resolve(path, "unbound", fb); ok {
		t.Error("unbound resolved with no fallback token")
	}
	if why := Explain(path, "unbound", "", fb); !strings.Contains(why, "AIMEM_OPENAI_API_KEY") {
		t.Errorf("unbound without fallback: %q", why)
	}
	if why := Explain(path, "unbound", "", Fallback{}); !strings.Contains(why, "no fallback endpoint") {
		t.Errorf("unbound with the zero Fallback: %q", why)
	}
}

// A registry that exists but does not parse must fail CLOSED: treating
// it as empty would make every bound model "unbound" and re-open the
// fallback routing the bindings exist to override.
func TestCorruptRegistryFailsClosed(t *testing.T) {
	path := regPath(t)
	if err := os.WriteFile(path, []byte(`{"providers":{"g":{"kind":"openai","token":"k"},}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Load(path)
	if r.LoadErr == nil {
		t.Fatal("parse error not recorded")
	}
	if _, ok := Resolve(path, "anything", envFallback); ok {
		t.Fatal("corrupt registry resolved a model via the fallback")
	}
	if why := Explain(path, "anything", "", envFallback); !strings.Contains(why, "could not be read") {
		t.Fatalf("reason: %q", why)
	}
	// For backend-picking callers a corrupt registry must read as
	// "bound, with a problem", never as "absent", which would license a
	// backend fallback.
	if _, isBound, why := ResolveBound(path, "anything"); !isBound || why == "" {
		t.Fatalf("corrupt registry must report bound-with-reason, got bound=%v why=%q", isBound, why)
	}
	// And a Save over it would erase the operator's providers.
	if err := r.Save(path); err == nil {
		t.Fatal("Save over an unreadable registry must be refused")
	}
}

// Only "file not found" may mean "no bindings"; any other read failure
// (permissions, a directory in the way) is a registry that cannot be
// trusted and must fail closed like a parse error.
func TestUnreadableRegistryFailsClosed(t *testing.T) {
	path := regPath(t)
	if err := os.Mkdir(path, 0o700); err != nil { // a directory where the file should be
		t.Fatal(err)
	}
	if r := Load(path); r.LoadErr == nil {
		t.Fatal("read error not recorded")
	}
	if _, ok := Resolve(path, "m", envFallback); ok {
		t.Fatal("unreadable registry resolved a model via the fallback")
	}
	if why := Explain(path, "m", "", envFallback); !strings.Contains(why, "could not be read") {
		t.Fatalf("reason: %q", why)
	}
	// And a genuinely missing file is still the fail-open case.
	if r := Load(regPath(t)); r.LoadErr != nil {
		t.Fatalf("missing registry must be absent, not an error: %v", r.LoadErr)
	}
}

// The three ResolveBound states, as backend-picking callers see them.
func TestResolveBoundStates(t *testing.T) {
	path := regPath(t)
	r := Load(path)
	r.Providers["g"] = Provider{Kind: "openai", Token: "k"}
	r.Providers["hollow"] = Provider{Kind: "openai"}
	r.Bind("good", "g", "")
	r.Bind("broken", "hollow", "")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	if ep, isBound, why := ResolveBound(path, "good"); !isBound || why != "" || ep.Token != "k" {
		t.Fatalf("good: %+v bound=%v why=%q", ep, isBound, why)
	}
	if _, isBound, why := ResolveBound(path, "broken"); !isBound || why == "" {
		t.Fatalf("broken must be bound-with-reason: bound=%v why=%q", isBound, why)
	}
	if _, isBound, why := ResolveBound(path, "absent"); isBound || why != "" {
		t.Fatalf("absent must be unbound without reason: bound=%v why=%q", isBound, why)
	}
}

func TestResolveAlias(t *testing.T) {
	path := regPath(t)
	r := Load(path)
	r.Providers["a"] = Provider{Kind: "openai", BaseURL: "https://a.example/v1", Token: "ka"}
	r.Providers["b"] = Provider{Kind: "openai", BaseURL: "https://b.example/v1", Token: "kb"}
	// Same upstream model through two providers under distinct local names.
	r.Bind("gpt4o-a", "a", "gpt-4o")
	r.Bind("gpt4o-b", "b", "gpt-4o")
	r.Bind("plain", "a", "plain") // upstream == alias normalizes to ""
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	if b := Load(path).Models["plain"]; b.Model != "" {
		t.Fatalf("upstream==alias should store empty, got %q", b.Model)
	}
	ep, ok := Resolve(path, "gpt4o-a", Fallback{})
	if !ok || ep.Model != "gpt-4o" || ep.Token != "ka" {
		t.Fatalf("alias a: %+v ok=%v", ep, ok)
	}
	if ep, ok = Resolve(path, "gpt4o-b", Fallback{}); !ok || ep.Model != "gpt-4o" || ep.Token != "kb" {
		t.Fatalf("alias b: %+v ok=%v", ep, ok)
	}
	if ep, ok = Resolve(path, "plain", Fallback{}); !ok || ep.Model != "plain" {
		t.Fatalf("plain: %+v ok=%v", ep, ok)
	}
}

func TestResolveNothingConfigured(t *testing.T) {
	if _, ok := Resolve(regPath(t), "m", Fallback{}); ok {
		t.Fatal("expected ok=false with no registry and no fallback")
	}
}

func TestProviderDefaultBaseURL(t *testing.T) {
	path := regPath(t)
	r := Load(path)
	r.Providers["p"] = Provider{Kind: "openai", Token: "k"}
	r.Bind("m", "p", "")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	if ep, ok := Resolve(path, "m", Fallback{}); !ok || ep.BaseURL != DefaultBaseURL {
		t.Fatalf("default base url: %+v ok=%v", ep, ok)
	}
	// A fallback without a URL assumes the default too.
	if ep, ok := Resolve(path, "unbound", Fallback{Token: "k"}); !ok || ep.BaseURL != DefaultBaseURL {
		t.Fatalf("fallback default base url: %+v ok=%v", ep, ok)
	}
}

// Fallback.Model replaces the payload name of every fallback-served model.
func TestFallbackModel(t *testing.T) {
	fb := Fallback{Token: "k", Model: "gpt-4o-mini"}
	if ep, ok := Resolve(regPath(t), "anything", fb); !ok || ep.Model != "gpt-4o-mini" {
		t.Fatalf("fallback model: %+v ok=%v", ep, ok)
	}
}

// Read applies the same fail-closed rule to a registry from a reader.
func TestReadFromReader(t *testing.T) {
	r := Read(strings.NewReader(`{"providers":{"g":{"kind":"openai","token":"k"}},"models":{"m":{"provider":"g"}}}`))
	if ep, ok := r.Resolve("m", Fallback{}); !ok || ep.Token != "k" {
		t.Fatalf("reader registry: %+v ok=%v", ep, ok)
	}
	bad := Read(strings.NewReader(`{`))
	if bad.LoadErr == nil {
		t.Fatal("parse error not recorded")
	}
	if _, ok := bad.Resolve("m", envFallback); ok {
		t.Fatal("corrupt registry from a reader resolved a model")
	}
	if empty := Read(nil); empty.LoadErr != nil || len(empty.Models) != 0 {
		t.Fatalf("nil reader: %+v", empty)
	}
}

// A providers.json exactly as aimem 0.10.13 wrote it (generated with that
// release's own provider.Save) loads and resolves the bindings that
// release resolved for it.
func TestAimem01013Fixture(t *testing.T) {
	path := filepath.Join("testdata", "providers-aimem-0.10.13.json")
	r := Load(path)
	if r.LoadErr != nil {
		t.Fatal(r.LoadErr)
	}
	want := map[string]struct {
		ep      Endpoint
		isBound bool
		reason  string
	}{
		"gpt4o-gw":               {Endpoint{Kind: "openai", BaseURL: "https://gateway.example/v1", Token: "sk-test-gateway", Model: "gpt-4o"}, true, ""},
		"gpt4o-oa":               {Endpoint{Kind: "openai", BaseURL: "https://api.openai.com/v1", Token: "sk-test-openai", Model: "gpt-4o"}, true, ""},
		"text-embedding-3-small": {Endpoint{Kind: "openai", BaseURL: "https://api.openai.com/v1", Token: "sk-test-openai", Model: "text-embedding-3-small"}, true, ""},
		"haiku":                  {Endpoint{Kind: "claude", Model: "haiku"}, true, ""},
		"sonnet-cli":             {Endpoint{Kind: "claude", Model: "sonnet"}, true, ""},
		"broken":                 {Endpoint{}, true, `model "broken" is bound to provider "hollow", which has no token stored — save the provider with its token (any non-empty value for an endpoint that needs none)`},
		"orphan":                 {Endpoint{}, true, `model "orphan" is bound to provider "vanished", which no longer exists`},
		"unbound":                {Endpoint{}, false, ""},
	}
	for m, w := range want {
		ep, isBound, reason := ResolveBound(path, m)
		if ep != w.ep || isBound != w.isBound || reason != w.reason {
			t.Errorf("%s: got %+v bound=%v reason=%q; want %+v bound=%v reason=%q", m, ep, isBound, reason, w.ep, w.isBound, w.reason)
		}
	}
	// Saving it again writes the same bytes aimem wrote.
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := regPath(t)
	if err := r.Save(out); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(out); string(again) != string(orig) {
		t.Fatalf("round trip changed the file:\n%s\nwant:\n%s", again, orig)
	}
}
