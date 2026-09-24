package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseGitProgress(t *testing.T) {
	tests := []struct {
		line       string
		wantOK     bool
		wantPhase  string
		wantPct    int
		wantDetail string
	}{
		{"Receiving objects:  45% (1500/3356), 297.00 MiB | 640.00 KiB/s", true, "Receiving objects", 45, "297.00 MiB | 640.00 KiB/s"},
		{"Resolving deltas: 100% (12/12), done.", true, "Resolving deltas", 100, ""},
		{"remote: Counting objects: 100% (5/5), done.", true, "Counting objects", 100, ""},
		{"Updating files:  62% (2081/3356)", true, "Updating files", 62, ""},
		{"Cloning into '/tmp/x'...", false, "", 0, ""},
		{"fatal: early EOF", false, "", 0, ""},
		{"remote: Enumerating objects: 3356, done.", false, "", 0, ""},
	}
	for _, tt := range tests {
		got, ok := parseGitProgress(tt.line)
		if ok != tt.wantOK {
			t.Errorf("parseGitProgress(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Phase != tt.wantPhase || got.Percent != tt.wantPct || got.Detail != tt.wantDetail {
			t.Errorf("parseGitProgress(%q) = %+v, want phase=%q pct=%d detail=%q", tt.line, got, tt.wantPhase, tt.wantPct, tt.wantDetail)
		}
	}
}

// git redraws progress in place with '\r', so the scanner must split on it.
func TestScanGitLinesSplitsOnCarriageReturn(t *testing.T) {
	input := "Receiving objects:  10% (1/10)\rReceiving objects:  20% (2/10)\rdone\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Split(scanGitLines)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	want := []string{"Receiving objects:  10% (1/10)", "Receiving objects:  20% (2/10)", "done"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProgressThrottle(t *testing.T) {
	now := time.Unix(1000, 0)
	th := &progressThrottle{now: func() time.Time { return now }}

	if !th.shouldReport(gitProgress{Phase: "Receiving objects", Percent: 1}) {
		t.Fatal("first update should be reported")
	}
	now = now.Add(time.Second)
	if th.shouldReport(gitProgress{Phase: "Receiving objects", Percent: 2}) {
		t.Fatal("update within the interval should be throttled")
	}
	now = now.Add(gitProgressLogInterval)
	if !th.shouldReport(gitProgress{Phase: "Receiving objects", Percent: 30}) {
		t.Fatal("update after the interval should be reported")
	}
	if !th.shouldReport(gitProgress{Phase: "Resolving deltas", Percent: 5}) {
		t.Fatal("a phase change should always be reported")
	}
	if !th.shouldReport(gitProgress{Phase: "Resolving deltas", Percent: 100}) {
		t.Fatal("phase completion should always be reported")
	}
}

type recordedProgress struct {
	mu   sync.Mutex
	msgs []string
	pcts []int
}

func (r *recordedProgress) reporter() *cloneReporter {
	return &cloneReporter{progress: func(stage string, pct int, message string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.msgs = append(r.msgs, message)
		r.pcts = append(r.pcts, pct)
	}}
}

// The whole point of the watchdog: a transfer that goes silent is killed
// after the stall timeout, not after the multi-minute hard ceiling.
func TestRunProgressCommandKillsStalledProcess(t *testing.T) {
	start := time.Now()
	err := runProgressCommand(
		context.Background(), nil, 400*time.Millisecond, nil,
		"sh", "-c", `printf 'Receiving objects:  10%% (1/10), 1.00 MiB | 1.00 MiB/s\r' >&2; sleep 30`,
	)
	if err == nil {
		t.Fatal("expected an error for a stalled process")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("error = %v, want it to mention the stall", err)
	}
	if !strings.Contains(err.Error(), "Receiving objects 10%") {
		t.Fatalf("error = %v, want it to include the last progress seen", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stall took %v to detect, want a few seconds", elapsed)
	}
}

func TestRunProgressCommandStallDisabled(t *testing.T) {
	err := runProgressCommand(context.Background(), nil, 0, nil, "sh", "-c", `sleep 0.3; echo ok`)
	if err != nil {
		t.Fatalf("stall=0 must not kill a slow-but-finishing process: %v", err)
	}
}

func TestRunProgressCommandReportsProgress(t *testing.T) {
	var rec recordedProgress
	err := runProgressCommand(
		context.Background(), rec.reporter(), 5*time.Second, nil,
		"sh", "-c", `printf 'Receiving objects:  50%% (5/10), 5.00 MiB | 2.00 MiB/s\rReceiving objects: 100%% (10/10), 10.00 MiB | 2.00 MiB/s, done.\n' >&2`,
	)
	if err != nil {
		t.Fatalf("runProgressCommand: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) == 0 {
		t.Fatal("expected at least one progress report")
	}
	last := rec.pcts[len(rec.pcts)-1]
	if want := cloneProgressStart + cloneProgressSpan; last != want {
		t.Fatalf("final job progress = %d, want %d (end of the clone slice)", last, want)
	}
}

// A failed clone must keep git's real message (withGitAuthHint matches on
// it) without dragging every progress line into the error.
func TestRunProgressCommandErrorKeepsGitMessageNotProgress(t *testing.T) {
	err := runProgressCommand(
		context.Background(), nil, 5*time.Second, nil,
		"sh", "-c", `p=Receiving; printf "$p objects:  10%% (1/10), 1.00 MiB | 1.00 MiB/s\r" >&2; echo "fatal: could not read Username for 'https://github.com'" >&2; exit 128`,
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "could not read Username") {
		t.Fatalf("error lost git's own message: %v", err)
	}
	if strings.Contains(err.Error(), "Receiving objects") {
		t.Fatalf("progress lines leaked into the error: %v", err)
	}
}

func TestGitTimeoutEnv(t *testing.T) {
	t.Setenv("GRAVYFLOW_GIT_CLONE_TIMEOUT", "")
	if got := gitTimeout(); got != gitCommandTimeout {
		t.Fatalf("default = %v, want %v", got, gitCommandTimeout)
	}
	t.Setenv("GRAVYFLOW_GIT_CLONE_TIMEOUT", "25m")
	if got := gitTimeout(); got != 25*time.Minute {
		t.Fatalf("configured = %v, want 25m", got)
	}
	t.Setenv("GRAVYFLOW_GIT_CLONE_TIMEOUT", "not-a-duration")
	if got := gitTimeout(); got != gitCommandTimeout {
		t.Fatalf("invalid falls back = %v, want %v", got, gitCommandTimeout)
	}

	t.Setenv("GRAVYFLOW_GIT_STALL_TIMEOUT", "")
	if got := gitStallTimeout(); got != defaultGitStallTimeout {
		t.Fatalf("stall default = %v, want %v", got, defaultGitStallTimeout)
	}
	t.Setenv("GRAVYFLOW_GIT_STALL_TIMEOUT", "0")
	if got := gitStallTimeout(); got != 0 {
		t.Fatalf("stall 0 must disable the watchdog, got %v", got)
	}
}

// End-to-end against real git: a clone made through runGitClone must stream
// "Receiving objects" progress to the reporter.
func TestGitCloneStreamsRealProgress(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(src, "init", "-q", "-b", "main")
	blob := make([]byte, 8<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", ".")
	run(src, "commit", "-q", "-m", "init")

	var rec recordedProgress
	ctx := withCloneReporter(context.Background(), rec.reporter().progress, nil)
	dest := filepath.Join(t.TempDir(), "checkout")
	if err := runGitClone(ctx, "file://"+src, dest, GitCloneOptions{}); err != nil {
		t.Fatalf("runGitClone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "data.bin")); err != nil {
		t.Fatalf("checkout is missing the file: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	joined := strings.Join(rec.msgs, "\n")
	if !strings.Contains(joined, "Receiving objects") {
		t.Fatalf("no clone progress was reported; got:\n%s", joined)
	}
}
