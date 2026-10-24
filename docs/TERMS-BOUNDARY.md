Checked: 2026-10-24

# Terms boundary

This document records what aiengine does and never does with model
vendors' services, and the vendors' own rules it was checked against. It
states facts and links to the vendors' pages; it is not legal advice.
Each user is responsible for using a vendor's service under that
vendor's terms. Where this document and a vendor's current page differ,
the vendor's page governs, and this document is out of date.

The date on the first line is the date every page below was last read.
CI fails when that date is more than 180 days old, so the boundary is
re-checked at least twice a year and before each release (see
[Re-check procedure](#re-check-procedure)).

## The kinds and what they do

aiengine has two backend kinds.

**The HTTP kind** (package `openai`) calls an OpenAI-compatible HTTP API
with an API key, or with a token for a proxy, that the user supplies to
the calling program.

**The CLI kind** (package `cli`) launches a vendor's own command-line
tool as the user installed and signed in to it, sends a prompt on
standard input, and reads the tool's printed output. Today the only CLI
vendor is Anthropic's Claude Code.

### The HTTP kind does

- send the caller's prompt, the model name and the caller's key to the
  endpoint the caller configured;
- read the response, including token usage and, from a LiteLLM proxy,
  the reported cost;
- space and retry calls when a pacer is configured.

### The HTTP kind never does

- choose an endpoint or a key on its own: both come from the caller;
- send the key anywhere other than the configured endpoint;
- store the key; it lives in the caller's configuration or registry
  file, which the caller owns.

### The CLI kind does

- run the vendor's unmodified CLI binary, found on `PATH` or at a path
  the caller names, with the vendor's documented headless flags;
- send the prompt on standard input and read the CLI's output on
  standard output and standard error;
- read the login state only from what the CLI prints (for Claude Code,
  "Not logged in · Please run /login" and similar), and report it as
  `login_expired`;
- run `--version` to report the CLI's version;
- start the CLI with the calling program's environment unchanged, so
  the CLI finds whatever sign-in its user configured for it (its own
  login, or a token the user set for it); aiengine neither reads nor
  adds any of it.

### The CLI kind never does

- read, store, copy or transmit a vendor's OAuth token, session token or
  credentials file, not even to read an expiry date; the source names no
  such file, and CI fails if it ever does;
- offer a vendor's sign-in inside another product: signing in happens
  in the vendor's own CLI, through the vendor's own flow, by the user;
- route several users through one subscription credential: the CLI runs
  as the local user, with that user's own sign-in;
- modify the vendor's CLI or remove any of its authentication methods.

## Vendor rules

### Anthropic (Claude Code)

Pages read on 2026-10-24:

- [Claude Code: Legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)
  (read 2026-10-24)
- [Claude Code: Authentication](https://code.claude.com/docs/en/authentication)
  (read 2026-10-24)
- [Consumer Terms of Service](https://www.anthropic.com/legal/consumer-terms),
  effective 2025-10-08 (read 2026-10-24)
- [Commercial Terms of Service](https://www.anthropic.com/legal/commercial-terms)
  (linked from the first page; governs Team, Enterprise and API use)

What they say, as read:

- OAuth authentication "is intended exclusively for purchasers of Claude
  Free, Pro, Max, Team, and Enterprise subscription plans and is
  designed to support ordinary use of Claude Code and other native
  Anthropic applications."
- "Anthropic does not permit third-party developers to offer Claude.ai
  login into their own applications, or to route requests through Free,
  Pro, or Max plan credentials on behalf of their users."
- "Developers may not collect, store, or intermediate Claude.ai
  credentials or session tokens — sign-in to a Claude account must
  complete through Anthropic's own flow."
- Developers building products or services should use API key
  authentication through Claude Console or a supported cloud provider.
- The rules do not prevent "an end user from signing in to the
  unmodified Claude Code binary with their own Claude subscription".
- "Advertised usage limits for Pro and Max plans assume ordinary,
  individual usage of Claude Code and the Agent SDK."
- `claude setup-token` generates "a one-year OAuth token" "for CI
  pipelines, scripts, or other environments where interactive browser
  login isn't available"; it "authenticates with your Claude
  subscription and requires a Pro, Max, Team, or Enterprise plan", and
  the CLI reads it from its own environment variable. It is an OAuth
  credential, so the OAuth rules above apply to it.
- The Consumer Terms forbid sharing "your Account login information,
  Anthropic API key, or Account credentials with anyone else", and
  forbid accessing the services "through automated or non-human means,
  whether through a bot, script, or otherwise", "except when you are
  accessing our Services via an Anthropic API Key or where we otherwise
  explicitly permit it".

What follows for aiengine: the CLI kind is for a subscriber's own use of
the Claude Code binary on their own machine, within ordinary individual
usage. A program that serves other people, or that is offered as a
product, uses the HTTP kind with an Anthropic API key (through an
OpenAI-compatible endpoint) or a cloud provider's credential instead.

### OpenAI (Codex CLI and the API)

Pages read on 2026-10-24:

- [Codex: Authentication](https://learn.chatgpt.com/docs/auth)
  (developers.openai.com/codex/auth redirects there; read 2026-10-24)
- [Terms of Use](https://openai.com/policies/row-terms-of-use/) and the
  [account sharing policy](https://help.openai.com/en/articles/10471989-openai-account-sharing-policy):
  both answered HTTP 403 to an automated read on 2026-10-24. Their
  wording is not quoted here; a person must read them at the next
  re-check.

What the Codex page says, as read:

- Codex signs in either "with ChatGPT for subscription access" or "with
  an API key for usage-based access".
- For headless machines it recommends device code authentication.
- Codex "caches login details locally in a plaintext file" or in the
  OS credential store; the page says to treat that file "like a
  password" and not to share it.
- "Use API key authentication for programmatic Codex CLI workflows,
  such as CI/CD jobs", and "API keys are still the recommended default
  for automation."

What follows for aiengine: aiengine has no Codex CLI kind yet. When one
is added, it runs the account holder's own signed-in Codex CLI, never
reads its cached login file, and is not a way to share one account
among people. Automation and anything that serves others use the HTTP
kind with an OpenAI API key.

### Google (Gemini)

Pages read on 2026-10-24:

- [Gemini Code Assist consumer accounts](https://developers.google.com/gemini-code-assist/docs/deprecations/code-assist-individuals),
  last updated 2026-10-09 (read 2026-10-24)
- [Gemini CLI: Terms of Service and Privacy Notice](https://github.com/google-gemini/gemini-cli/blob/main/docs/resources/tos-privacy.md)
  (read 2026-10-24)

What they say, as read:

- From 2026-06-18 Gemini Code Assist stopped serving the individual,
  Google AI Pro and Google AI Ultra tiers; "This also applies to usage
  of Gemini CLI", and "you can no longer use the Login with Google
  option to access the IDE extensions or Gemini CLI." Standard and
  Enterprise subscriptions follow a separate deprecation schedule.
- The Gemini CLI terms say that using third-party software to reach the
  services behind Gemini CLI through its OAuth (the page's example:
  "using OpenClaw with Gemini CLI OAuth") "is a violation of applicable
  terms and policies" and "may be grounds for suspension or termination
  of your account."

What follows for aiengine: Gemini is reached with an API key only, over
an OpenAI-compatible endpoint, through the HTTP kind. aiengine will not
drive the Gemini CLI's Google sign-in.

## What leaves the machine

Every call to a hosted model sends the prompt, and everything the
calling program put into it, to that model's vendor, or to the proxy in
between and then to the vendor. That holds for every kind: free or
paid, API key or subscription CLI. From then on the vendor's terms,
retention rules and training rules govern that data, not aiengine's.

aiengine adds nothing of its own to a request beyond what the caller
gives it: the prompt, the model name, the caller's key (HTTP kind), and
the CLI's own command line (CLI kind). It sends no telemetry.

Confidential, commercial or personal data belongs on a local endpoint
(an OpenAI-compatible server on the same machine or network, such as
Ollama or vLLM, configured as an HTTP provider), or with no hosted
provider at all. Choosing where data may go is the calling program's
and its user's decision.

## Re-check procedure

Before each release, and at least every 180 days (CI enforces the
latter):

1. Read every page linked above in a browser. For each vendor, look
   for changes to: who may use subscription sign-in (OAuth) and for
   what; any rule on third-party tools, wrappers or automation driving
   the vendor's CLI; rules on sharing accounts or credentials; usage
   limits and "ordinary use" language; whether a sign-in method was
   added, retired or restricted (as Google's was on 2026-06-18).
2. Search each vendor's documentation for new pages on the same
   subjects, and add them here with their read date.
3. Read the pages this document could not quote (OpenAI's Terms of Use
   and account sharing policy, as of 2026-10-24) and quote them.
4. If a rule changed, update the vendor section, the does and
   never-does lists, and the README's disclaimer, and record the
   change in CHANGELOG.md. If a change makes a kind's use against the
   rules, remove or restrict that kind before releasing.
5. Set the date on the first line to the day of the reading, and the
   read date beside every link.
