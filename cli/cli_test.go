package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aiengine"
	"github.com/BlackVS/aiengine/pace"
)

// The fake CLI is this test binary re-executed: AIENGINE_FAKE_CLI picks
// what it plays (see TestMain). fakeRunner points a Runner at it.
func fakeRunner(t *testing.T, mode string) *Runner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIENGINE_FAKE_CLI", mode)
	return &Runner{Vendor: Claude{}, Bin: exe, WorkDir: t.TempDir()}
}

// loggedOut is what Claude Code 2.1.310 printed on stdout, with exit
// status 1, for `claude -p --model haiku --output-format json` run with
// an empty configuration directory (observed 2026-10-24, verbatim except
// the session and message ids, zeroed). The result text comes after
// more than a kilobyte of other fields.
const loggedOut = `{"duration_api_ms":0,"stop_reason":"stop_sequence","session_id":"00000000-0000-0000-0000-000000000000",` +
	`"total_cost_usd":0,"usage":{"output_tokens_details":{"thinking_tokens":0},"input_tokens":0,` +
	`"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,` +
	`"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard",` +
	`"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"",` +
	`"iterations":[],"speed":"standard","fallback_credit":null},"modelUsage":{},"permission_denials":[],` +
	`"terminal_reason":"api_error","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required",` +
	`"subagent_stats":{"spawned":0,"requested":{"background":0,"foreground":0,"unset":0},` +
	`"started_in_background":0,"max_depth":0,"spawned_by_subagents":0,"completed":0,"failed":0,` +
	`"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},` +
	`"by_type":{}},"safety_stops":0,"is_error":true,"num_turns":1,"subtype":"success","api_error_status":null,` +
	`"result":"Not logged in · Please run /login","type":"result","duration_ms":95,` +
	`"uuid":"00000000-0000-0000-0000-000000000000","queued_turn_count":0,"result_index":0}`

func TestCompleteSendsArgsAndStdin(t *testing.T) {
	r := fakeRunner(t, "echo")
	prompt := "line one\nline two with \"quotes\" and %PATH% & | <>"
	res, u, err := r.Complete(context.Background(), "", prompt)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
		Dir   string   `json:"dir"`
	}
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("helper reply: %q: %v", res, err)
	}
	// The command line aimem 0.10.13 runs, with its default model.
	if want := "-p --model haiku --output-format json"; strings.Join(got.Args, " ") != want {
		t.Fatalf("args: %q, want %q", got.Args, want)
	}
	if got.Stdin != prompt {
		t.Fatalf("stdin round trip mangled: %q", got.Stdin)
	}
	if !sameDir(got.Dir, r.WorkDir) {
		t.Fatalf("work dir: %q, want %q", got.Dir, r.WorkDir)
	}
	if u.InputTokens != 3+20+100 || u.OutputTokens != 7 || u.CostUSD != 0.01 {
		t.Fatalf("usage not parsed: %+v", u)
	}
	if _, _, err := r.Complete(context.Background(), "sonnet", "x"); err != nil {
		t.Fatal(err)
	}
}

func sameDir(a, b string) bool {
	ea, _ := filepath.EvalSymlinks(a)
	eb, _ := filepath.EvalSymlinks(b)
	return strings.EqualFold(filepath.Clean(ea), filepath.Clean(eb))
}

// erroredTurn is an is_error result that reports its spend.
const erroredTurn = `{"is_error":true,"result":"boom","total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":1}}`

func TestCompleteLoginExpired(t *testing.T) {
	for _, mode := range []string{"logged-out", "logged-out-exit0"} {
		r := fakeRunner(t, mode)
		_, _, err := r.Complete(context.Background(), "", "x")
		if !errors.Is(err, aiengine.ErrLoginExpired) {
			t.Fatalf("%s: want ErrLoginExpired, got %v", mode, err)
		}
		if aiengine.AuthOf(err) != aiengine.AuthLoginExpired || !strings.Contains(err.Error(), "Not logged in") {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestCompleteFailure(t *testing.T) {
	r := fakeRunner(t, "fail")
	_, _, err := r.Complete(context.Background(), "", "x")
	if err == nil || errors.Is(err, aiengine.ErrLoginExpired) || aiengine.AuthOf(err) != aiengine.AuthError {
		t.Fatalf("want a plain failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 2") || !strings.Contains(err.Error(), "boom on stderr") {
		t.Fatalf("failure must carry the exit and stderr: %v", err)
	}
}

// An errored turn reports its spend whether the CLI then exits 0 or 1.
func TestCompleteErroredTurnKeepsUsage(t *testing.T) {
	for _, mode := range []string{"errored", "errored-exit1"} {
		r := fakeRunner(t, mode)
		_, u, err := r.Complete(context.Background(), "", "x")
		if err == nil || aiengine.AuthOf(err) != aiengine.AuthError {
			t.Fatalf("%s: err=%v", mode, err)
		}
		if u.InputTokens != 5 || u.OutputTokens != 1 || u.CostUSD != 0.01 {
			t.Fatalf("%s: usage lost: %+v", mode, u)
		}
	}
}

func TestCompleteUsageLimitWidensPacing(t *testing.T) {
	r := fakeRunner(t, "limited")
	r.Pacer = pace.New(pace.Options{})
	if _, _, err := r.Complete(context.Background(), "", "x"); err == nil {
		t.Fatal("want an error")
	}
	if s := r.Pacer.Status(); s.Blocks != 1 {
		t.Fatalf("usage limit not counted as a block: %+v", s)
	}
}

// Numbers and ids elsewhere in a failed turn's JSON are not a rate
// limit: only the result's own text is classified.
func TestCompleteFailureIgnoresIncidentalDigits(t *testing.T) {
	r := fakeRunner(t, "errored-429-digits")
	r.Pacer = pace.New(pace.Options{})
	_, _, err := r.Complete(context.Background(), "", "x")
	if err == nil || !strings.Contains(err.Error(), "boom") || aiengine.AuthOf(err) != aiengine.AuthError {
		t.Fatalf("want the turn's own error, got %v", err)
	}
	if s := r.Pacer.Status(); s.Blocks != 0 {
		t.Fatalf("an incidental 429 in the JSON counted as a block: %+v", s)
	}
}

func TestVersion(t *testing.T) {
	r := fakeRunner(t, "echo")
	v, err := r.Version(context.Background())
	if err != nil || v != "2.1.310 (Claude Code)" {
		t.Fatalf("version: %q err=%v", v, err)
	}
	res := aiengine.Probe(context.Background(), r, "")
	if res.Version != "2.1.310 (Claude Code)" || res.Auth != aiengine.AuthOK || res.Err != nil {
		t.Fatalf("probe: %+v", res)
	}
	if !strings.Contains(res.Answer, aiengine.ProbePrompt) { // the echo helper returns its stdin
		t.Fatalf("probe answer: %q", res.Answer)
	}
}

func TestProbeLoggedOut(t *testing.T) {
	res := aiengine.Probe(context.Background(), fakeRunner(t, "logged-out"), "")
	if res.Auth != aiengine.AuthLoginExpired || res.Answer != "" {
		t.Fatalf("probe: %+v", res)
	}
}

func TestParseCountsCachedTokens(t *testing.T) {
	// The headless CLI's top-level input_tokens is only the UNCACHED
	// slice; a real extraction rides prompt caching almost entirely.
	out := []byte(`{"type":"result","is_error":false,"result":"[]",
		"total_cost_usd":0.0123,
		"usage":{"input_tokens":42,"cache_creation_input_tokens":18000,
		         "cache_read_input_tokens":9500,"output_tokens":310}}`)
	content, u, err := Claude{}.Parse(out)
	if err != nil || content != "[]" {
		t.Fatalf("parse: %q err=%v", content, err)
	}
	if u.InputTokens != 42+18000+9500 {
		t.Fatalf("input tokens must include cache creation+read: %d", u.InputTokens)
	}
	if u.OutputTokens != 310 || u.CostUSD != 0.0123 {
		t.Fatalf("usage: %+v", u)
	}
	// An errored turn still reports its usage (spend happened).
	out = []byte(`{"is_error":true,"result":"boom","usage":{"input_tokens":5,"output_tokens":1}}`)
	if _, u, err := (Claude{}).Parse(out); err == nil || u.InputTokens != 5 {
		t.Fatalf("errored turn: err=%v usage=%+v", err, u)
	}
	if _, _, err := (Claude{}).Parse([]byte("not json")); !errors.Is(err, ErrBadOutput) {
		t.Fatalf("non-JSON output must fail with ErrBadOutput: %v", err)
	}
}

func TestClaudeClassifiers(t *testing.T) {
	for _, s := range []string{
		"Not logged in · Please run /login",
		"Login expired · Please run /login",
		"OAuth token has expired. Please obtain a new token or refresh your existing token.",
		`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
		"Invalid API key · Please run /login",
	} {
		if !(Claude{}).LoginExpired(s) {
			t.Errorf("LoginExpired(%q) = false", s)
		}
	}
	for _, s := range []string{"Claude AI usage limit reached|1761300000", `API Error: 429 {"error":"rate_limit"}`, "Overloaded"} {
		if !(Claude{}).Blocked(s) || (Claude{}).LoginExpired(s) {
			t.Errorf("Blocked(%q) wrong", s)
		}
	}
	if (Claude{}).LoginExpired("connection refused") || (Claude{}).Blocked("connection refused") {
		t.Error("ordinary errors are neither")
	}
}

func TestRunnerWithoutVendor(t *testing.T) {
	if _, _, err := (&Runner{}).Complete(context.Background(), "", "x"); err == nil {
		t.Fatal("a Runner without a Vendor must fail")
	}
}

// TestCompleteTimeout: a hung CLI is killed at the bound. The bound is
// measured from when the helper began running: creating the process is
// the OS's time, not the kill's, and on a loaded Windows machine it once
// took seconds. Returning before the helper's 30s sleep is the proof.
func TestCompleteTimeout(t *testing.T) {
	r := fakeRunner(t, "sleep")
	stamp, started := helperStamp(t, "helper-started")
	t.Setenv("AIENGINE_FAKE_CLI_STAMP", stamp)
	r.Timeout = 300 * time.Millisecond
	begin := time.Now()
	_, _, err := r.Complete(context.Background(), "", "prompt")
	done := time.Now()
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must wrap context.DeadlineExceeded for callers: %v", err)
	}
	if total := done.Sub(begin); total >= 30*time.Second {
		t.Fatalf("Complete took %s: the hung helper ran to its own exit, it was never killed", total)
	}
	if at, _, ok := started(); ok {
		if ran := done.Sub(at); ran > 5*time.Second {
			t.Fatalf("kill took %s after the helper started: process not bounded", ran)
		}
	}
}

// A caller's own cancellation ends the turn too, and is not reported
// as the runner's timeout.
func TestCompleteCallerCancel(t *testing.T) {
	r := fakeRunner(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := r.Complete(ctx, "", "prompt")
	if err == nil || strings.Contains(err.Error(), "timed out after") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller cancel: %v", err)
	}
}

// TestCompleteTimeoutWithGrandchild: ctx expiry kills only the direct
// child, and a grandchild inheriting stdout keeps cmd.Output() blocked
// past the kill; only WaitDelay force-closes the pipes.
func TestCompleteTimeoutWithGrandchild(t *testing.T) {
	r := fakeRunner(t, "spawn")
	r.WaitDelay = 1 * time.Second
	stamp, started := helperStamp(t, "grandchild-started")
	t.Setenv("AIENGINE_FAKE_CLI_STAMP", stamp)
	// The timeout leaves the helper ample time to start and spawn its
	// grandchild before the kill; the kill still lands long before the
	// grandchild would let go of the pipe on its own.
	r.Timeout = 3 * time.Second
	_, _, err := r.Complete(context.Background(), "", "prompt")
	done := time.Now()
	born, pid, ok := started()
	if ok {
		// On Windows a live grandchild wedges TempDir removal and the test
		// binary's deletion; it is ended here rather than slept out.
		t.Cleanup(func() {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
				p.Wait()
			}
		})
	}
	if err == nil {
		t.Fatal("want an error from the killed CLI")
	}
	if !ok {
		t.Fatal("the grandchild never started: nothing held the pipe, so the test proved nothing")
	}
	if held := done.Sub(born); held >= 30*time.Second {
		t.Fatalf("Complete blocked %s after the grandchild started: it held the pipe past WaitDelay", held)
	}
}

// helperStamp names a file the helper writes when it starts, and returns
// a reader for its start time and PID.
func helperStamp(t *testing.T, name string) (string, func() (time.Time, int, bool)) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	return p, func() (time.Time, int, bool) {
		raw, err := os.ReadFile(p)
		if err != nil {
			return time.Time{}, 0, false
		}
		var ns int64
		var pid int
		if _, err := fmt.Sscan(string(raw), &ns, &pid); err != nil {
			return time.Time{}, 0, false
		}
		return time.Unix(0, ns), pid, true
	}
}

// stampHelperStart records when the helper began running and its PID,
// written aside and renamed into place so a kill mid-write leaves none.
func stampHelperStart() {
	if p := os.Getenv("AIENGINE_FAKE_CLI_STAMP"); p != "" {
		if os.WriteFile(p+".tmp", []byte(strconv.FormatInt(time.Now().UnixNano(), 10)+" "+strconv.Itoa(os.Getpid())), 0o600) == nil {
			os.Rename(p+".tmp", p)
		}
	}
}

// TestMain doubles as the fake CLI. Re-executed with AIENGINE_FAKE_CLI
// set and the claude command line (-p ... or --version), it plays:
// "echo" (a clean turn whose result is its argv, stdin and directory),
// "logged-out" (the observed signed-out output, exit 1),
// "logged-out-exit0" (the same result with exit 0), "errored" (an
// is_error turn with usage), "fail" (stderr text, exit 2), "limited" (a
// usage-limit message, exit 1), "sleep" (hangs 30s), and "spawn" (hangs
// after starting a grandchild, "hold", that keeps the stdout pipe 60s).
// It gates every test run in this package, so keep the guard exact.
func TestMain(m *testing.M) {
	mode := os.Getenv("AIENGINE_FAKE_CLI")
	if mode == "" || len(os.Args) < 2 || (os.Args[1] != "-p" && os.Args[1] != "--version") {
		os.Exit(m.Run())
	}
	if os.Args[1] == "--version" {
		fmt.Println("2.1.310 (Claude Code)")
		os.Exit(0)
	}
	switch mode {
	case "echo":
		in, _ := io.ReadAll(os.Stdin)
		wd, _ := os.Getwd()
		res, _ := json.Marshal(map[string]any{"args": os.Args[1:], "stdin": string(in), "dir": wd})
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"result": string(res), "is_error": false, "total_cost_usd": 0.01,
			"usage": map[string]any{"input_tokens": 3, "cache_creation_input_tokens": 20,
				"cache_read_input_tokens": 100, "output_tokens": 7},
		})
	case "logged-out":
		fmt.Print(loggedOut)
		os.Exit(1)
	case "logged-out-exit0":
		fmt.Print(loggedOut)
	case "errored-429-digits":
		fmt.Print(`{"duration_ms":1429,"session_id":"04290000-0000","is_error":true,"result":"boom"}`)
		os.Exit(1)
	case "errored":
		fmt.Print(erroredTurn)
	case "errored-exit1":
		fmt.Print(erroredTurn)
		os.Exit(1)
	case "fail":
		fmt.Fprint(os.Stderr, "boom on stderr")
		os.Exit(2)
	case "limited":
		fmt.Fprint(os.Stderr, "Claude AI usage limit reached|1761300000")
		os.Exit(1)
	case "spawn":
		exe, _ := os.Executable()
		child := exec.Command(exe, "-p")
		child.Env = append(os.Environ(), "AIENGINE_FAKE_CLI=hold")
		child.Dir = os.TempDir() // never hold the test's TempDir
		child.Stdout = os.Stdout // inherit the pipe: the orphan that used to wedge Output()
		if err := child.Start(); err != nil {
			os.Exit(3) // loud: no grandchild stamp, and the test says so
		}
		time.Sleep(30 * time.Second)
	case "hold":
		stampHelperStart()
		time.Sleep(60 * time.Second)
	default: // "sleep"
		stampHelperStart()
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}
