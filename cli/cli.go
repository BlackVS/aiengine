// Package cli runs a vendor's own headless CLI as a model backend.
//
// The runner launches the binary the user installed, signed in to and
// configured, sends the prompt on stdin and parses the CLI's own output.
// It never opens, reads, copies or transmits the vendor's credentials or
// configuration files: the CLI handles its login itself, and the login
// state is read only from what the CLI prints. See docs/TERMS-BOUNDARY.md
// for the vendors' rules on such use.
//
// One Vendor implementation describes each CLI (Claude today); further
// vendors are further implementations of the same small interface.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/BlackVS/aiengine"
	"github.com/BlackVS/aiengine/pace"
)

// Vendor describes one headless CLI: how to call it and how to read it.
type Vendor interface {
	// Name is the vendor CLI's name for messages ("claude").
	Name() string
	// DefaultBin is the executable looked up on PATH when Runner.Bin is
	// empty; DefaultModel is used when the caller names no model.
	DefaultBin() string
	DefaultModel() string
	// Args is the command line after the binary for one headless turn.
	// The prompt is not part of it: it always travels on stdin.
	Args(model string) []string
	// Parse decodes a finished run's stdout into the answer and usage.
	// An errored turn returns its usage too (the spend happened).
	Parse(stdout []byte) (string, aiengine.Usage, error)
	// LoginExpired reports whether the CLI's output says its own login
	// is missing or no longer valid.
	LoginExpired(output string) bool
	// Blocked reports whether a failure looks rate-limit shaped.
	Blocked(output string) bool
	// VersionArgs is the command line that prints the CLI's version.
	VersionArgs() []string
}

const (
	// DefaultTimeout bounds one headless turn.
	DefaultTimeout = 5 * time.Minute
	// DefaultWaitDelay bounds the pipe close after a timeout kill.
	DefaultWaitDelay = 10 * time.Second

	versionTimeout = 30 * time.Second
)

// Runner runs headless turns of one vendor CLI. It implements
// aiengine.Completer and aiengine.Versioner.
type Runner struct {
	Vendor Vendor
	Bin    string // "" = Vendor.DefaultBin(), looked up on PATH
	// WorkDir is the CLI's working directory. Run it outside any real
	// project, so the turns do not land in that project's history.
	WorkDir string
	Timeout time.Duration // per turn; 0 = DefaultTimeout
	// WaitDelay bounds how long the runner waits for the CLI's pipes to
	// close after a kill; 0 = DefaultWaitDelay.
	WaitDelay time.Duration
	// Pacer spaces turns like any other model call. nil runs unpaced,
	// which suits one human-triggered probe: a diagnostic must not queue
	// behind the batch pacer during the very outage it is diagnosing.
	// The CLI kind never retries; a failed turn goes back to the caller.
	Pacer *pace.Pacer
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return r.Vendor.DefaultBin()
}

func (r *Runner) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	// On ctx expiry Go kills only the DIRECT child; a CLI that spawns
	// subprocesses (on Windows an npm .cmd shim guarantees a grandchild)
	// leaves an orphan holding the inherited stdout, which keeps
	// cmd.Output() blocked past the kill. WaitDelay force-closes the
	// pipes so the timeout actually bounds the call.
	cmd.WaitDelay = r.WaitDelay
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	cmd.Dir = r.WorkDir
	return cmd
}

// Complete runs one headless turn of prompt by model ("" = the vendor's
// default), bounded by Timeout. A login the CLI reports as missing or
// expired returns an error wrapping aiengine.ErrLoginExpired.
//
// The prompt travels over STDIN, never argv: prompts realistically
// exceed Windows' 32K CreateProcess limit, and for an npm .cmd shim
// os/exec documents that cmd.exe "has a different unquoting algorithm"
// the caller must handle, so prompt text as argv would reach cmd.exe's
// parser unescaped (CVE-2024-24576's class). Stdin has neither problem.
func (r *Runner) Complete(ctx context.Context, model, prompt string) (string, aiengine.Usage, error) {
	var u aiengine.Usage
	if r.Vendor == nil {
		return "", u, errors.New("cli: Runner has no Vendor")
	}
	if model == "" {
		model = r.Vendor.DefaultModel()
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if err := r.Pacer.Wait(ctx); err != nil {
		return "", u, err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := r.command(tctx, r.Vendor.Args(model))
	cmd.Stdin = strings.NewReader(prompt)
	name := r.Vendor.Name()
	out, err := cmd.Output()
	if err != nil {
		// A SUCCESSFUL run whose lingering subprocess held the pipe past
		// WaitDelay surfaces as ErrWaitDelay with the full result already
		// in out; failing here would discard a paid-for completion.
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return r.finish(out)
		}
		if ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
			// Wrap both: DeadlineExceeded so callers can errors.Is the
			// timeout, and the exec error for the exit detail.
			return "", u, fmt.Errorf("%s CLI timed out after %s: %w (%w)", name, timeout, context.DeadlineExceeded, err)
		}
		if ctx.Err() != nil {
			return "", u, fmt.Errorf("%s CLI: %w (%w)", name, ctx.Err(), err)
		}
		// A CLI may report API failures on STDOUT (the claude CLI prints
		// its JSON result with is_error) and exit nonzero: surface both
		// streams, and classify them whole. The message is clipped, but
		// the classification is not: in the claude CLI's result the
		// telling text comes after a kilobyte of other fields.
		stdout := strings.TrimSpace(string(out))
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		diag := pace.Clip(stdout, 200)
		if stderr != "" {
			if diag != "" {
				diag += " | "
			}
			diag += pace.Clip(stderr, 200)
		}
		// An errored turn's own text is the clearest detail when the
		// output parses as one.
		if _, _, perr := r.Vendor.Parse(out); perr != nil && r.Vendor.LoginExpired(perr.Error()) {
			diag = perr.Error()
		}
		whole := stdout + "\n" + stderr
		switch {
		case diag == "":
			return "", u, fmt.Errorf("%s CLI failed: %w", name, err)
		case r.Vendor.LoginExpired(whole):
			return "", u, fmt.Errorf("%s CLI: %w — sign in again with the vendor's CLI (%s)", name, aiengine.ErrLoginExpired, diag)
		case r.Vendor.Blocked(whole):
			r.Pacer.Penalize(name + " CLI: " + diag)
		}
		return "", u, fmt.Errorf("%s CLI failed: %w (%s)", name, err, diag)
	}
	return r.finish(out)
}

// finish parses a completed run and keeps the pacing protocol honest:
// clean completions decay any penalty, and a rate-limit shaped errored
// turn widens it.
func (r *Runner) finish(out []byte) (string, aiengine.Usage, error) {
	res, u, err := r.Vendor.Parse(out)
	switch {
	case err == nil:
		r.Pacer.Recover()
	case r.Vendor.LoginExpired(err.Error()):
		return "", u, fmt.Errorf("%s CLI: %w — sign in again with the vendor's CLI (%v)", r.Vendor.Name(), aiengine.ErrLoginExpired, err)
	case r.Vendor.Blocked(err.Error()):
		r.Pacer.Penalize(r.Vendor.Name() + " CLI: " + pace.Clip(err.Error(), 200))
	}
	return res, u, err
}

// Version runs the CLI's version command and returns its first line of
// output, trimmed ("2.1.310 (Claude Code)").
func (r *Runner) Version(ctx context.Context) (string, error) {
	if r.Vendor == nil {
		return "", errors.New("cli: Runner has no Vendor")
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := r.command(ctx, r.Vendor.VersionArgs()).Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", r.Vendor.Name(), err)
	}
	line, _, _ := bytes.Cut(bytes.TrimSpace(out), []byte("\n"))
	return string(bytes.TrimSpace(line)), nil
}
