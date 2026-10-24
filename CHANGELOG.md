# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

The first version: model access moved out of aimem (from its `master`
after v0.10.13) into a public module of its own.

### Added

- Package `provider`: the model registry (`Provider`, `Binding`,
  `Registry`, `Load`, `Save`, `Bind`, `Lookup`, `Resolve`, `Explain`,
  `ResolveBound`) with aimem's fail-closed load. The registry path and
  the fallback endpoint are the caller's parameters (`Load(path)`,
  `Read(io.Reader)`, `Fallback{BaseURL, Token, Model, Source}`); the
  module reads no environment. A providers.json written by aimem 0.10.13
  loads and resolves the same bindings (tested with a fixture written by
  that release's own `Save`).
- Package `openai`: one `Client` with `Complete(ctx, model, prompt)` and
  `Embed(ctx, model, texts)`, the request and response shapes of aimem's
  chat-completions and embeddings clients, usage counts, the LiteLLM
  cost header, the embeddings width guard, and `DefaultBaseURL`
  overridable per client.
- Package `cli`: `Runner` runs a vendor's headless CLI behind one small
  `Vendor` interface; `Claude` runs `claude -p --model <model>
  --output-format json` with the prompt on stdin, as aimem 0.10.13 does,
  parses its JSON result and usage, recognises a missing or expired
  login from the CLI's own output (`aiengine.ErrLoginExpired`), and
  reports `--version`. It opens no credentials file.
- Package `pace`: aimem's per-call pacing as a `Pacer` value with the
  caller's interval, retries, state directory, clock and log function.
- Root package `aiengine`: `Completer`, `Versioner`, `Usage`,
  `AuthState` and `Probe`.
- `docs/TERMS-BOUNDARY.md`, the README disclaimer, and CI guards: the
  Go source names no vendor credentials file, and the boundary document
  was checked within 180 days.

### Changed from aimem's code

- `Endpoint.FromEnv` is `Endpoint.FromFallback`.
- `Registry.Save` refuses to overwrite a registry that failed to load
  (aimem's callers already checked `LoadErr` before saving).
- Calls take a `context.Context`; pacing waits and retry sleeps end with
  it.
- A nil `*pace.Pacer` means unpaced with no retries (aimem's `NoPace`).
