package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// LIVE BUILD LOGS
// ============================================================================
//
// Build output (git, docker build, nixpacks) is split into lines and, per
// line, published on the job's status channel (the one the dashboard's job
// WebSocket already listens to) and appended to a capped Redis list. The list
// is the backlog: a client that connects mid-build, or opens a failed
// deployment later, still sees what happened.

const (
	deploymentJobLogsKey  = "deployment:job:%s:logs"
	jobLogMessageType     = "log"
	maxJobLogLines        = 2000
	maxJobLogLineLength   = 2000
	jobLogRetention       = 24 * time.Hour
	jobLogPublishTimeout  = 2 * time.Second
	failureLogTailLines   = 200
)

// ansiEscape matches terminal color/cursor sequences, which some builders
// (nixpacks) emit even when not writing to a terminal.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// JobLogMessage is one build log line. Type distinguishes it from the
// DeploymentJobStatus messages sharing the same channel.
type JobLogMessage struct {
	Type   string    `json:"type"`
	JobID  string    `json:"jobId"`
	UserID string    `json:"userId"`
	Seq    int64     `json:"seq"`
	Line   string    `json:"line"`
	Time   time.Time `json:"time"`
}

// jobLogWriter is an io.Writer that publishes each complete line it receives.
// Safe for concurrent use (a command's stdout and stderr write at once).
// Writes never fail: losing a log line must not fail the build.
type jobLogWriter struct {
	m      *DeploymentJobManager
	jobID  string
	userID string

	mu  sync.Mutex
	buf []byte
	seq int64
}

func (m *DeploymentJobManager) newJobLogWriter(jobID string, userID string) *jobLogWriter {
	return &jobLogWriter{m: m, jobID: jobID, userID: userID}
}

func (w *jobLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexAny(string(w.buf), "\r\n")
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		w.publishLocked(line)
	}
	// A builder that never writes a newline must not grow the buffer forever.
	if len(w.buf) > maxJobLogLineLength {
		w.publishLocked(string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// Printf writes one GravyFlow-generated line (stage headers, outcome).
func (w *jobLogWriter) Printf(format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// Close publishes any trailing partial line.
func (w *jobLogWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.publishLocked(string(w.buf))
		w.buf = w.buf[:0]
	}
}

func (w *jobLogWriter) publishLocked(line string) {
	line = strings.TrimRight(ansiEscape.ReplaceAllString(line, ""), " \t")
	if line == "" || w.m == nil || w.m.redisClient == nil {
		return
	}
	if len(line) > maxJobLogLineLength {
		line = line[:maxJobLogLineLength] + "…"
	}

	w.seq++
	data, err := json.Marshal(JobLogMessage{
		Type:   jobLogMessageType,
		JobID:  w.jobID,
		UserID: w.userID,
		Seq:    w.seq,
		Line:   line,
		Time:   time.Now().UTC(),
	})
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), jobLogPublishTimeout)
	defer cancel()

	key := fmt.Sprintf(deploymentJobLogsKey, w.jobID)
	pipe := w.m.redisClient.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.LTrim(ctx, key, -maxJobLogLines, -1)
	pipe.Expire(ctx, key, jobLogRetention)
	pipe.Publish(ctx, w.m.channelKey(w.jobID), data)
	_, _ = pipe.Exec(ctx)
}

// JobLogBacklog returns the last `limit` stored log lines for a job, oldest
// first (limit <= 0 means everything kept).
func (m *DeploymentJobManager) JobLogBacklog(ctx context.Context, jobID string, limit int) ([]JobLogMessage, error) {
	if m == nil || m.redisClient == nil {
		return nil, fmt.Errorf("deployment job manager is not initialized")
	}
	start := int64(0)
	if limit > 0 {
		start = int64(-limit)
	}
	raw, err := m.redisClient.LRange(ctx, fmt.Sprintf(deploymentJobLogsKey, strings.TrimSpace(jobID)), start, -1).Result()
	if err != nil {
		return nil, err
	}
	lines := make([]JobLogMessage, 0, len(raw))
	for _, item := range raw {
		var msg JobLogMessage
		if json.Unmarshal([]byte(item), &msg) == nil {
			lines = append(lines, msg)
		}
	}
	return lines, nil
}

// logSink returns w, or io.Discard when there's nowhere to send build output.
func logSink(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
