// Package provider resolves which endpoint serves a model. A registry
// file, at a path the caller chooses (it holds tokens: keep it private
// to the host, mode 0600, never synced), maps model names to named
// providers. An optional Fallback endpoint, supplied by the caller,
// serves models that have NO binding, so a host without the file keeps
// working from its configuration; a model that IS bound resolves only
// through its binding.
//
// The registry fails closed: a missing file is an empty registry, but a
// file that cannot be read or parsed resolves nothing, because treating
// it as empty would quietly route every bound model to the fallback.
package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/BlackVS/aiengine/openai"
)

// DefaultBaseURL is where an "openai" provider points when it names no
// URL of its own, and what a Fallback without a BaseURL assumes.
const DefaultBaseURL = openai.DefaultBaseURL

// Provider kinds.
const (
	KindOpenAI = "openai" // an OpenAI-compatible HTTP endpoint (package openai)
	KindClaude = "claude" // the headless claude CLI (package cli)
)

// Provider is one named endpoint.
type Provider struct {
	Kind    string `json:"kind"`               // KindOpenAI | KindClaude
	BaseURL string `json:"base_url,omitempty"` // openai kind; empty = DefaultBaseURL
	Token   string `json:"token,omitempty"`    // openai kind; never leaves this host
}

// Binding maps a local model name to a provider. Model is the upstream
// name sent in API payloads; empty means the local name IS the upstream
// name. Distinct local aliases let the same upstream model ride several
// providers (gpt4o-a and gpt4o-b both -> "gpt-4o").
type Binding struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// Registry is the on-disk shape of the registry file.
type Registry struct {
	Providers map[string]Provider `json:"providers"`
	Models    map[string]Binding  `json:"models"`
	// LoadErr is set when the registry exists but cannot be read or
	// parsed. Such a registry resolves nothing (see the package comment),
	// and a Save over it would erase the operator's providers.
	LoadErr error `json:"-"`
}

// Fallback is the caller's endpoint for models without a binding (aimem
// fills it from its own environment variables). The zero value means no
// fallback: an unbound model does not resolve.
type Fallback struct {
	BaseURL string // empty = DefaultBaseURL
	Token   string // empty = no fallback
	// Model, when set, is the upstream name sent for every model the
	// fallback serves; empty sends the requested name itself.
	Model string
	// Source names where the fallback comes from, for the reason given
	// when it is not configured (aimem: "AIMEM_OPENAI_API_KEY").
	// Empty reads "no fallback endpoint".
	Source string
}

// Load reads the registry at path. A MISSING file yields an empty
// registry; any other failure (unreadable, a directory, present but
// unparseable) is recorded in LoadErr.
func Load(path string) *Registry {
	f, err := os.Open(path)
	switch {
	case err == nil:
		defer f.Close()
		return read(f, path)
	case os.IsNotExist(err):
		return read(nil, path)
	default:
		return withMaps(&Registry{LoadErr: err})
	}
}

// Read parses a registry from r, with the same fail-closed rule: a read
// or parse error is recorded in LoadErr. A nil r is an empty registry.
func Read(r io.Reader) *Registry { return read(r, "registry") }

func read(r io.Reader, name string) *Registry {
	reg := &Registry{}
	if r != nil {
		raw, err := io.ReadAll(r)
		if err == nil {
			err = json.Unmarshal(raw, reg)
		}
		if err != nil {
			reg = &Registry{LoadErr: fmt.Errorf("%s: %w", name, err)}
		}
	}
	return withMaps(reg)
}

func withMaps(r *Registry) *Registry {
	if r.Providers == nil {
		r.Providers = map[string]Provider{}
	}
	if r.Models == nil {
		r.Models = map[string]Binding{}
	}
	return r
}

// Save writes the registry to path atomically at mode 0600. It refuses a
// registry that failed to load, which would overwrite the file with an
// empty one.
func (r *Registry) Save(path string) error {
	if r.LoadErr != nil {
		return fmt.Errorf("refusing to save over an unreadable registry: %w", r.LoadErr)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Bind maps a local model name to a named provider; upstream is the
// payload model name ("" = same as the local name).
func (r *Registry) Bind(model, providerName, upstream string) {
	if upstream == model {
		upstream = ""
	}
	r.Models[model] = Binding{Provider: providerName, Model: upstream}
}

// Endpoint is a resolved model -> endpoint mapping. Model is the name to
// put in API payloads (the binding's upstream name, or the requested
// name itself).
type Endpoint struct {
	Kind         string
	BaseURL      string
	Token        string
	Model        string
	FromFallback bool // served by the caller's Fallback, not a registry binding
}

// bound classifies a model's registry binding: the endpoint it names,
// or the reason it cannot serve. isBound=false means the model has no
// binding at all (the fallback's territory). ONE decision tree, so the
// verdict and its explanation cannot drift apart.
func (r *Registry) bound(model string) (ep Endpoint, isBound bool, reason string) {
	b, ok := r.Models[model]
	if !ok {
		return Endpoint{}, false, ""
	}
	upstream := b.Model
	if upstream == "" {
		upstream = model
	}
	p, ok := r.Providers[b.Provider]
	switch {
	case !ok:
		return Endpoint{}, true, fmt.Sprintf("model %q is bound to provider %q, which no longer exists", model, b.Provider)
	case p.Kind == KindClaude:
		return Endpoint{Kind: KindClaude, Model: upstream}, true, ""
	case p.Token == "":
		// The registry has never served a tokenless endpoint; auth-free
		// proxies take any non-empty placeholder.
		return Endpoint{}, true, fmt.Sprintf("model %q is bound to provider %q, which has no token stored — save the provider with its token (any non-empty value for an endpoint that needs none)", model, b.Provider)
	}
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return Endpoint{Kind: KindOpenAI, BaseURL: base, Token: p.Token, Model: upstream}, true, ""
}

// ResolveBound resolves only through an explicit registry binding, with
// no fallback. Callers that let the binding's kind PICK the backend
// (claude CLI or openai HTTP) must use this and keep the three states
// apart: isBound=false is the only state in which a backend fallback is
// legitimate; isBound=true with a reason is a binding (or registry) that
// cannot be honored, and falling back to ANY backend from there serves
// the model from the wrong service. An unreadable registry reports as
// bound-with-reason for every model, because nothing can be known about
// its bindings.
func (r *Registry) ResolveBound(model string) (ep Endpoint, isBound bool, reason string) {
	if r.LoadErr != nil {
		return Endpoint{}, true, fmt.Sprintf("provider registry could not be read (%v) — fix it by hand; no model resolves until it does", r.LoadErr)
	}
	return r.bound(model)
}

// Lookup is the one resolution verdict for a model: its endpoint, or a
// reason (in words an operator can act on) why none serves it. A bound
// model resolves only through its binding: the binding is the operator's
// statement of where the model lives, and quietly serving it from the
// fallback made a misconfigured provider look healthy while an unrelated
// service answered in its place. Unbound models use fb. wantKind, when
// set, also rejects an endpoint of the wrong kind (a claude binding
// where an OpenAI-compatible endpoint is required), saying so.
func (r *Registry) Lookup(model, wantKind string, fb Fallback) (Endpoint, string) {
	ep, isBound, reason := r.ResolveBound(model)
	if !isBound {
		if fb.Token == "" {
			src := fb.Source
			if src == "" {
				return Endpoint{}, fmt.Sprintf("model %q has no binding and no fallback endpoint is configured", model)
			}
			return Endpoint{}, fmt.Sprintf("model %q has no binding and %s is unset", model, src)
		}
		ep = Endpoint{Kind: KindOpenAI, BaseURL: fb.BaseURL, Token: fb.Token, Model: model, FromFallback: true}
		if ep.BaseURL == "" {
			ep.BaseURL = DefaultBaseURL
		}
		if fb.Model != "" {
			ep.Model = fb.Model
		}
	}
	if reason != "" {
		return Endpoint{}, reason
	}
	if wantKind != "" && ep.Kind != wantKind {
		return Endpoint{}, fmt.Sprintf("model %q resolves to a %s endpoint, but a %s one is required here", model, ep.Kind, wantKind)
	}
	return ep, ""
}

// Resolve is Lookup without a kind requirement; ok=false means no
// endpoint serves the model.
func (r *Registry) Resolve(model string, fb Fallback) (Endpoint, bool) {
	ep, reason := r.Lookup(model, "", fb)
	return ep, reason == ""
}

// Explain is the reason half of Lookup: "" when the model resolves.
func (r *Registry) Explain(model, wantKind string, fb Fallback) string {
	_, reason := r.Lookup(model, wantKind, fb)
	return reason
}

// Lookup, Resolve, Explain and ResolveBound below read the registry at
// path afresh on every call, so an edit to the file takes effect at once.

// Lookup is Registry.Lookup on the registry at path.
func Lookup(path, model, wantKind string, fb Fallback) (Endpoint, string) {
	return Load(path).Lookup(model, wantKind, fb)
}

// Resolve is Registry.Resolve on the registry at path.
func Resolve(path, model string, fb Fallback) (Endpoint, bool) {
	return Load(path).Resolve(model, fb)
}

// Explain is Registry.Explain on the registry at path.
func Explain(path, model, wantKind string, fb Fallback) string {
	return Load(path).Explain(model, wantKind, fb)
}

// ResolveBound is Registry.ResolveBound on the registry at path.
func ResolveBound(path, model string) (Endpoint, bool, string) {
	return Load(path).ResolveBound(model)
}
