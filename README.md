# aiengine

Model access for Go programs, using only the standard library:

- **provider**: a registry file that says which endpoint serves a model,
  failing closed when the file cannot be read;
- **openai**: a client for OpenAI-compatible HTTP endpoints (chat
  completions and embeddings);
- **cli**: a runner for a vendor's own headless CLI (Claude Code today);
- **pace**: per-call spacing, backoff retries and adaptive slow-down
  after rate-limit blocks;
- the root package **aiengine**: the kind interface every backend
  implements (`Completer`), the `Usage` it reports, and `Probe`.

The module reads no environment variables and opens no vendor
credentials file. Paths, keys and settings arrive as parameters from the
calling program.

```sh
go get github.com/BlackVS/aiengine@latest
```

Go 1.26 or newer.

## The kinds

| Kind | Package | Talks to | Authenticates with |
| --- | --- | --- | --- |
| `openai` | `openai` | any OpenAI-compatible HTTP API: OpenAI, a LiteLLM or vLLM proxy, Ollama, a vendor's compatible endpoint | the API key or proxy token the caller gives it |
| `claude` | `cli` | the `claude` binary the user installed | the CLI's own sign-in, which the module never touches |

Both implement `aiengine.Completer`:

```go
Complete(ctx context.Context, model, prompt string) (string, aiengine.Usage, error)
```

`openai.Client` also has `Embed(ctx, model, texts)`.

## The registry

The registry is a JSON file at a path the caller chooses. It holds
tokens, so keep it private to the host (mode 0600; `Save` writes it so)
and never sync it.

```json
{
  "providers": {
    "openai": { "kind": "openai", "token": "sk-..." },
    "gateway": { "kind": "openai", "base_url": "https://llm.example/v1", "token": "..." },
    "claude-cli": { "kind": "claude" }
  },
  "models": {
    "gpt4o": { "provider": "openai", "model": "gpt-4o" },
    "gpt4o-gw": { "provider": "gateway", "model": "gpt-4o" },
    "text-embedding-3-small": { "provider": "openai" },
    "haiku": { "provider": "claude-cli" }
  }
}
```

A binding's `model` is the upstream name sent in requests; when it is
absent, the local name is sent. An `openai` provider without `base_url`
points at `https://api.openai.com/v1`.

Resolution rules:

- A bound model resolves only through its binding. A binding to a
  provider that is missing, or that has no token, does not resolve, and
  the reason says why.
- An unbound model resolves through the caller's optional `Fallback`
  endpoint, or not at all.
- A missing file is an empty registry. A file that exists but cannot be
  read or parsed resolves nothing, and `Save` refuses to overwrite it.

## Resolve a model and call it

An API-key provider:

```go
reg := provider.Load("/path/to/providers.json")
ep, why := reg.Lookup("gpt4o", provider.KindOpenAI, provider.Fallback{})
if why != "" {
	return errors.New(why)
}
pacer := pace.New(pace.Options{Interval: pace.DefaultInterval, Retries: pace.DefaultRetries})
c := &openai.Client{BaseURL: ep.BaseURL, Token: ep.Token, Pacer: pacer}
answer, usage, err := c.Complete(ctx, ep.Model, "Summarise this in one line: ...")
```

Embeddings through the same kind:

```go
c := &openai.Client{BaseURL: ep.BaseURL, Token: ep.Token, Dimensions: 768, Pacer: pacer}
vectors, usage, err := c.Embed(ctx, ep.Model, []string{"first text", "second text"})
```

A fallback for unbound models, filled from the calling program's own
configuration:

```go
fb := provider.Fallback{BaseURL: cfg.BaseURL, Token: cfg.APIKey, Source: "the API key setting"}
ep, ok := provider.Resolve("/path/to/providers.json", "gpt-4o-mini", fb)
```

A CLI provider. The CLI kind runs the user's own signed-in Claude Code
for that user's own use; read the [disclaimer](#disclaimer) and
[docs/TERMS-BOUNDARY.md](docs/TERMS-BOUNDARY.md) before using it.

```go
ep, isBound, why := reg.ResolveBound("haiku")
if isBound && why == "" && ep.Kind == provider.KindClaude {
	r := &cli.Runner{Vendor: cli.Claude{}, WorkDir: scratchDir, Pacer: pacer}
	answer, usage, err := r.Complete(ctx, ep.Model, prompt)
	if errors.Is(err, aiengine.ErrLoginExpired) {
		// the user signs in again in the claude CLI itself
	}
}
```

The prompt goes to the CLI on standard input, never on the command line.

## Probe

`aiengine.Probe` sends one fixed prompt through any `Completer` and
reports the answer, the backend's version where it has one (the CLI's
`--version`), and the auth state: `ok`, `login_expired` or `error`.

```go
res := aiengine.Probe(ctx, r, "haiku")
fmt.Println(res.Auth, res.Version, res.Answer)
```

## Pacing

`pace.New(pace.Options{...})` returns a pacer that every caller in a
process shares. It spaces calls by `Interval` plus an adaptive penalty,
retries a blocked HTTP call up to `Retries` times with backoff, and
widens the penalty after each rate-limit block (it halves again on each
success). With `StateDir` the penalty persists across processes in
`llmrate.json`. A nil `*pace.Pacer` means unpaced, with no retries.

## DISCLAIMER

aiengine calls model vendors' services with your keys, or runs a
vendor's CLI with your own sign-in. You are responsible for using each
service under its vendor's terms. Subscription sign-ins (such as a
Claude subscription in Claude Code) are for the subscriber's own use;
aiengine never reads, stores or forwards a vendor's credentials, never
offers a vendor's sign-in inside another product, and must not be used
to route several people through one subscription. Anything that serves
other people uses API keys. Every call to a hosted model sends the
prompt to that vendor. The vendors' rules as read, with dated links, and
what leaves the machine: [docs/TERMS-BOUNDARY.md](docs/TERMS-BOUNDARY.md).
This is not legal advice.

## Development

```sh
go build ./...
go test ./...
gofmt -l .          # must print nothing
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
sh scripts/check-no-credential-files.sh
sh scripts/check-boundary-date.sh
```

CI runs all of these on Linux and Windows, on Go 1.26.0 and the newest
1.26 patch.

## License

[PolyForm Noncommercial 1.0.0](LICENSE). The code comes from
[aimem](https://github.com/BlackVS/aimem), whose notice the license
carries.
