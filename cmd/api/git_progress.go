package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ============================================================================
// CLONE CONFIGURATION
// ============================================================================
//
//	GRAVYFLOW_GIT_CLONE_TIMEOUT   hard ceiling for one clone/fetch (default 10m)
//	GRAVYFLOW_GIT_STALL_TIMEOUT   kill git if it prints nothing for this long
//	                              (default 2m; 0 disables). This is what turns
//	                              a hung transfer into a fast, clear failure
//	                              instead of waiting out the full ceiling.

const (
	defaultGitStallTimeout = 2 * time.Minute
	gitProgressLogInterval = 5 * time.Second
	// A clone's byte-transfer phase is mapped onto this slice of the job's
	// overall progress bar (cloning starts at 15, building starts at 50).
	cloneProgressStart = 15
	cloneProgressSpan  = 30
	maxGitErrorTail    = 16 << 10
)

func gitTimeout() time.Duration {
	d, err := durationFromEnv("GRAVYFLOW_GIT_CLONE_TIMEOUT", gitCommandTimeout)
	if err != nil || d == 0 {
		return gitCommandTimeout
	}
	return d
}

func gitStallTimeout() time.Duration {
	d, err := durationFromEnv("GRAVYFLOW_GIT_STALL_TIMEOUT", defaultGitStallTimeout)
	if err != nil {
		return defaultGitStallTimeout
	}
	return d
}

// ============================================================================
// PROGRESS REPORTER (carried on the context)
// ============================================================================

// cloneReporter receives clone progress. progress updates the deployment
// job's stage/percent/message; logs is the user-facing deploy log.
type cloneReporter struct {
	progress func(stage string, pct int, message string)
	logs     io.Writer
}

type cloneReporterKey struct{}

func withCloneReporter(ctx context.Context, progress func(stage string, pct int, message string), logs io.Writer) context.Context {
	if progress == nil && logs == nil {
		return ctx
	}
	return context.WithValue(ctx, cloneReporterKey{}, &cloneReporter{progress: progress, logs: logs})
}

func cloneReporterFrom(ctx context.Context) *cloneReporter {
	r, _ := ctx.Value(cloneReporterKey{}).(*cloneReporter)
	return r
}

// note sends a one-off message (e.g. the repository size check result).
func (r *cloneReporter) note(message string) {
	log.Printf("[git] %s", message)
	if r == nil {
		return
	}
	if r.progress != nil {
		// The worker's progress callback already appends "==> message" to the
		// deploy log; writing it again here duplicated every line.
		r.progress("cloning", cloneProgressStart, message)
	} else if r.logs != nil {
		fmt.Fprintf(r.logs, "    %s\n", message)
	}
}

// ============================================================================
// PROGRESS PARSING
// ============================================================================

type gitProgress struct {
	Phase   string
	Percent int
	Detail  string // e.g. "297.00 MiB | 640.00 KiB/s"
}

var gitProgressRe = regexp.MustCompile(
	`^(?:remote: )?(Enumerating objects|Counting objects|Compressing objects|Receiving objects|Resolving deltas|Updating files|Checking out files):\s+(\d+)%\s*(?:\([^)]*\))?,?\s*(.*)$`,
)

func parseGitProgress(line string) (gitProgress, bool) {
	m := gitProgressRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return gitProgress{}, false
	}
	pct, err := strconv.Atoi(m[2])
	if err != nil {
		return gitProgress{}, false
	}
	detail := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[3]), "done."))
	detail = strings.TrimRight(detail, ", ")
	return gitProgress{Phase: m[1], Percent: pct, Detail: detail}, true
}

func (p gitProgress) String() string {
	if p.Detail == "" {
		return fmt.Sprintf("%s %d%%", p.Phase, p.Percent)
	}
	return fmt.Sprintf("%s %d%% (%s)", p.Phase, p.Percent, p.Detail)
}

// scanGitLines splits on '\r' as well as '\n': git redraws progress lines in
// place using carriage returns, so a plain line scanner would see one
// enormous "line" until the whole transfer finished.
func scanGitLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// progressThrottle decides when a parsed progress update is worth reporting:
// on a phase change, on completion of a phase, or every gitProgressLogInterval.
type progressThrottle struct {
	now       func() time.Time
	lastPhase string
	lastText  string
	lastAt    time.Time
}

func (t *progressThrottle) shouldReport(p gitProgress) bool {
	// "0%" lines are just each phase announcing itself.
	if p.Percent == 0 {
		return false
	}
	now := t.now()
	// git prints a phase's final line twice (with and without "done"); an
	// identical repeat carries no information.
	if text := p.String(); text == t.lastText {
		return false
	} else {
		t.lastText = text
	}
	if p.Phase != t.lastPhase || p.Percent >= 100 || now.Sub(t.lastAt) >= gitProgressLogInterval {
		t.lastPhase = p.Phase
		t.lastAt = now
		return true
	}
	return false
}

func (r *cloneReporter) report(p gitProgress) {
	message := p.String()
	log.Printf("[git] %s", message)
	if r == nil {
		return
	}
	if r.progress != nil {
		pct := cloneProgressStart
		if p.Phase == "Receiving objects" {
			pct += p.Percent * cloneProgressSpan / 100
		}
		r.progress("cloning", pct, message)
	} else if r.logs != nil {
		fmt.Fprintf(r.logs, "    %s\n", message)
	}
}

// ============================================================================
// RUNNER WITH PROGRESS + STALL WATCHDOG
// ============================================================================

// runGitCommandProgress runs a network-bound git command (clone/fetch) that
// was invoked with --progress. It streams progress to the console, the job
// status and the deploy log, and kills git when it goes silent for
// GRAVYFLOW_GIT_STALL_TIMEOUT — a stalled transfer used to sit until the full
// timeout with no output at all.
//
// Progress lines are kept out of the returned error (a 1 GB clone prints
// hundreds of them); everything else git printed is preserved, since
// withGitAuthHint matches on it.
func runGitCommandProgress(ctx context.Context, env []string, args ...string) error {
	return runProgressCommand(ctx, cloneReporterFrom(ctx), gitStallTimeout(), env, "git", args...)
}

func runProgressCommand(parent context.Context, reporter *cloneReporter, stall time.Duration, env []string, name string, args ...string) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	if env != nil {
		cmd.Env = env
	}
	// git forks helpers (git-remote-https, index-pack) that inherit the
	// stderr pipe. Killing only the parent leaves them holding it open, so
	// the read below would block until they exit on their own — defeating
	// the stall watchdog. Run git in its own process group and kill the
	// whole group on cancel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s %s: stderr pipe: %w", name, strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s %s failed to start: %w", name, strings.Join(args, " "), err)
	}

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var lastProgressLine atomic.Value
	lastProgressLine.Store("")

	var tail bytes.Buffer
	throttle := &progressThrottle{now: time.Now}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		scanner.Split(scanGitLines)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			lastActivity.Store(time.Now().UnixNano())
			if p, ok := parseGitProgress(line); ok {
				lastProgressLine.Store(p.String())
				if throttle.shouldReport(p) {
					reporter.report(p)
				}
				continue
			}
			fmt.Fprintln(os.Stderr, line)
			if tail.Len() < maxGitErrorTail {
				tail.WriteString(line)
				tail.WriteByte('\n')
			}
		}
	}()

	stopWatchdog := make(chan struct{})
	if stall > 0 {
		interval := stall / 4
		if interval > 5*time.Second {
			interval = 5 * time.Second
		}
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stopWatchdog:
					return
				case <-ticker.C:
					idle := time.Since(time.Unix(0, lastActivity.Load()))
					if idle >= stall {
						last, _ := lastProgressLine.Load().(string)
						cancel(fmt.Errorf("stalled: no output for %s (last progress: %q); set GRAVYFLOW_GIT_STALL_TIMEOUT to tune", stall.Round(time.Second), last))
						return
					}
				}
			}
		}()
	}

	// Belt and braces: if something escaped the process group and still
	// holds the pipe after cancellation, don't block on it forever.
	go func() {
		select {
		case <-readerDone:
		case <-ctx.Done():
			select {
			case <-readerDone:
			case <-time.After(5 * time.Second):
				_ = stderrPipe.Close()
			}
		}
	}()

	// The pipe must be fully drained before Wait, which closes it.
	<-readerDone
	close(stopWatchdog)
	waitErr := cmd.Wait()
	if waitErr == nil {
		return nil
	}

	label := fmt.Sprintf("%s %s", name, strings.Join(args, " "))
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%s %w", label, cause)
	}
	if trimmed := strings.TrimSpace(tail.String()); trimmed != "" {
		return fmt.Errorf("%s failed: %w: %s", label, waitErr, trimmed)
	}
	return fmt.Errorf("%s failed: %w", label, waitErr)
}
