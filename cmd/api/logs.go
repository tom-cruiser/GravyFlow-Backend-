package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	maxLogLinesPerMessage   = 100
	maxWebSocketConnections = 100
	logBufferSize           = 1024 * 1024 // 1MB
	defaultTailLines        = 100
	writeWait               = 10 * time.Second
	pongWait                = 60 * time.Second
	pingPeriod              = (pongWait * 9) / 10
	maxMessageSize          = 512
)

// ============================================================================
// TYPES
// ============================================================================

type logStreamMessage struct {
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	Timestamp time.Time `json:"timestamp"`
}

type logStreamOptions struct {
	Tail      int
	Follow    bool
	ShowStdout bool
	ShowStderr bool
	Filter    string
	Since     time.Time
}

type websocketLineWriter struct {
	conn    *websocket.Conn
	stream  string
	mu      *sync.Mutex
	buffer  bytes.Buffer
	options logStreamOptions
}

// ============================================================================
// WEBSOCKET UPGRADER
// ============================================================================

var logsWebsocketUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	Subprotocols:    []string{"json"},
}

// ============================================================================
// WEBSOCKET LINE WRITER
// ============================================================================

func (w *websocketLineWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	written := len(p)
	
	// Apply filter if specified
	if w.options.Filter != "" && !strings.Contains(string(p), w.options.Filter) {
		return written, nil
	}

	for len(p) > 0 {
		newlineIndex := bytes.IndexByte(p, '\n')
		if newlineIndex < 0 {
			// Check buffer size limit
			if w.buffer.Len()+len(p) > logBufferSize {
				w.buffer.Reset()
			}
			_, _ = w.buffer.Write(p)
			break
		}

		_, _ = w.buffer.Write(p[:newlineIndex])
		if err := w.flushLine(); err != nil {
			return 0, err
		}
		p = p[newlineIndex+1:]
	}

	return written, nil
}

func (w *websocketLineWriter) flushRemaining() error {
	if w.buffer.Len() == 0 {
		return nil
	}
	return w.flushLine()
}

func (w *websocketLineWriter) flushLine() error {
	line := strings.TrimRight(w.buffer.String(), "\r")
	w.buffer.Reset()
	if line == "" {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Set write deadline
	if err := w.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}

	return w.conn.WriteJSON(logStreamMessage{
		Stream:    w.stream,
		Line:      line,
		Timestamp: time.Now().UTC(),
	})
}

// ============================================================================
// CONNECTION MANAGER
// ============================================================================

type ConnectionManager struct {
	connections map[string][]*websocket.Conn
	mu          sync.RWMutex
	maxConns    int
}

var connManager = &ConnectionManager{
	connections: make(map[string][]*websocket.Conn),
	maxConns:    maxWebSocketConnections,
}

func (cm *ConnectionManager) Add(deploymentID string, conn *websocket.Conn) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Check connection limit
	if len(cm.connections[deploymentID]) >= cm.maxConns {
		return fmt.Errorf("max connections reached for deployment %s", deploymentID)
	}

	cm.connections[deploymentID] = append(cm.connections[deploymentID], conn)
	return nil
}

func (cm *ConnectionManager) Remove(deploymentID string, conn *websocket.Conn) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	conns := cm.connections[deploymentID]
	for i, c := range conns {
		if c == conn {
			cm.connections[deploymentID] = append(conns[:i], conns[i+1:]...)
			break
		}
	}
	if len(cm.connections[deploymentID]) == 0 {
		delete(cm.connections, deploymentID)
	}
}

func (cm *ConnectionManager) Broadcast(deploymentID string, message interface{}) {
	cm.mu.RLock()
	conns := cm.connections[deploymentID]
	cm.mu.RUnlock()

	for _, conn := range conns {
		if err := conn.WriteJSON(message); err != nil {
			cm.Remove(deploymentID, conn)
		}
	}
}

// ============================================================================
// MAIN HANDLER
// ============================================================================

func streamAppLogsHandler(c *gin.Context) {
	// Authenticate user
	user, err := authenticateUser(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// Get deployment
	deploymentID := strings.TrimSpace(c.Param("id"))
	if deploymentID == "" {
		sendBadRequest(c, "deployment id is required", nil)
		return
	}

	deployment, err := deploymentStore.GetDeploymentForUser(c.Request.Context(), user.ID, deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_not_found", "details": err.Error()})
		return
	}

	// Parse options
	opts := parseLogOptions(c)

	// Check container status
	containerID := strings.TrimSpace(deployment.ContainerID)
	if containerID == "" {
		handleNoContainer(c, "container not ready yet — deployment is still building.")
		return
	}

	// Create context with cancel
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	// Upgrade to WebSocket
	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Add to connection manager
	if err := connManager.Add(deploymentID, conn); err != nil {
		_ = conn.WriteJSON(gin.H{"error": err.Error()})
		return
	}
	defer connManager.Remove(deploymentID, conn)

	// Setup WebSocket with ping/pong
	setupWebSocket(conn)

	// Connect to Docker
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		_ = conn.WriteJSON(gin.H{"error": fmt.Sprintf("create docker client: %v", err)})
		return
	}
	defer dockerClient.Close()

	// Get container logs
	logStream, err := dockerClient.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: opts.ShowStdout,
		ShowStderr: opts.ShowStderr,
		Follow:     opts.Follow,
		Tail:       fmt.Sprintf("%d", opts.Tail),
		Timestamps: true,
		Since:      opts.Since.Format(time.RFC3339),
	})
	if err != nil {
		_ = conn.WriteJSON(gin.H{"error": fmt.Sprintf("open container logs: %v", err)})
		return
	}
	defer logStream.Close()

	// Setup writers
	writerMu := &sync.Mutex{}
	stdoutWriter := &websocketLineWriter{
		conn:    conn,
		stream:  "stdout",
		mu:      writerMu,
		options: opts,
	}
	stderrWriter := &websocketLineWriter{
		conn:    conn,
		stream:  "stderr",
		mu:      writerMu,
		options: opts,
	}

	// Start reading logs
	var wg sync.WaitGroup
	wg.Add(2)

	// Log reader goroutine
	go func() {
		defer wg.Done()
		_, err := stdcopy.StdCopy(stdoutWriter, stderrWriter, logStream)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			_ = conn.WriteJSON(gin.H{"error": err.Error()})
		}
	}()

	// Message reader goroutine (for client disconnection detection)
	go func() {
		defer wg.Done()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Flush remaining on completion
	go func() {
		wg.Wait()
		_ = stdoutWriter.flushRemaining()
		_ = stderrWriter.flushRemaining()
		_ = conn.WriteJSON(gin.H{"message": "log stream ended"})
		conn.Close()
	}()

	// Wait for cancellation
	<-ctx.Done()
}

// ============================================================================
// AUTHENTICATION
// ============================================================================

func authenticateUser(c *gin.Context) (UserRecord, error) {
	// Try query parameter first (for WebSocket)
	if bearerToken := strings.TrimSpace(c.Query("token")); bearerToken != "" {
		claims, err := parseAndValidateToken(bearerToken, tokenTypeAccess)
		if err != nil {
			return UserRecord{}, fmt.Errorf("invalid_token: %w", err)
		}
		user, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
		if err != nil {
			return UserRecord{}, fmt.Errorf("user_not_found: %w", err)
		}
		return user, nil
	}

	// Try Authorization header
	user, ok := currentAuthUser(c)
	if !ok {
		return UserRecord{}, fmt.Errorf("unauthorized")
	}
	return user, nil
}

// ============================================================================
// OPTION PARSING
// ============================================================================

func parseLogOptions(c *gin.Context) logStreamOptions {
	opts := logStreamOptions{
		Tail:       defaultTailLines,
		Follow:     true,
		ShowStdout: true,
		ShowStderr: true,
	}

	// Parse tail parameter
	if tail := c.Query("tail"); tail != "" {
		if t, err := fmt.Sscanf(tail, "%d", &opts.Tail); err == nil && t == 1 {
			if opts.Tail > 10000 {
				opts.Tail = 10000
			}
		}
	}

	// Parse follow parameter
	if follow := c.Query("follow"); follow != "" {
		opts.Follow = strings.ToLower(follow) != "false"
	}

	// Parse streams
	if streams := c.Query("streams"); streams != "" {
		opts.ShowStdout = strings.Contains(streams, "stdout")
		opts.ShowStderr = strings.Contains(streams, "stderr")
	}

	// Parse filter
	if filter := c.Query("filter"); filter != "" {
		opts.Filter = filter
	}

	// Parse since
	if since := c.Query("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			opts.Since = t
		}
	}

	return opts
}

// ============================================================================
// WEBSOCKET SETUP
// ============================================================================

func setupWebSocket(conn *websocket.Conn) {
	conn.SetReadLimit(maxMessageSize)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Start ping ticker
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()
}

// ============================================================================
// NO CONTAINER HANDLER
// ============================================================================

func handleNoContainer(c *gin.Context, message string) {
	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Send info message
	_ = conn.WriteJSON(logStreamMessage{
		Stream:    "info",
		Line:      message,
		Timestamp: time.Now().UTC(),
	})

	// Send end marker
	_ = conn.WriteJSON(gin.H{"message": "log stream ended"})
}

// ============================================================================
// BATCH PROCESSING
// ============================================================================

type batchWriter struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	batch  []logStreamMessage
	maxSize int
}

func newBatchWriter(conn *websocket.Conn) *batchWriter {
	return &batchWriter{
		conn:    conn,
		batch:   make([]logStreamMessage, 0, maxLogLinesPerMessage),
		maxSize: maxLogLinesPerMessage,
	}
}

func (bw *batchWriter) Write(stream string, line string) error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	bw.batch = append(bw.batch, logStreamMessage{
		Stream:    stream,
		Line:      line,
		Timestamp: time.Now().UTC(),
	})

	if len(bw.batch) >= bw.maxSize {
		return bw.flush()
	}
	return nil
}

func (bw *batchWriter) flush() error {
	if len(bw.batch) == 0 {
		return nil
	}

	if err := bw.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}

	// Send all messages in batch
	for _, msg := range bw.batch {
		if err := bw.conn.WriteJSON(msg); err != nil {
			return err
		}
	}

	bw.batch = bw.batch[:0]
	return nil
}

// ============================================================================
// METRICS (Optional)
// ============================================================================

type logStreamMetrics struct {
	mu              sync.RWMutex
	totalConnections int64
	activeConnections int64
	totalMessages   int64
	totalErrors     int64
}

var metrics = &logStreamMetrics{}

func (m *logStreamMetrics) IncrementConnections() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalConnections++
	m.activeConnections++
}

func (m *logStreamMetrics) DecrementConnections() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeConnections--
}

func (m *logStreamMetrics) IncrementMessages() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalMessages++
}

func (m *logStreamMetrics) IncrementErrors() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalErrors++
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Basic log streaming (last 100 lines):
   ws://localhost:8080/api/deployments/123/logs?token=jwt

2. Stream only last 50 lines:
   ws://localhost:8080/api/deployments/123/logs?token=jwt&tail=50

3. Filter logs by keyword:
   ws://localhost:8080/api/deployments/123/logs?token=jwt&filter=error

4. Show only stderr:
   ws://localhost:8080/api/deployments/123/logs?token=jwt&streams=stderr

5. Logs since specific time:
   ws://localhost:8080/api/deployments/123/logs?token=jwt&since=2024-01-15T10:00:00Z

6. Disable follow (get logs and close):
   ws://localhost:8080/api/deployments/123/logs?token=jwt&follow=false
*/

// ============================================================================
// CLIENT-SIDE EXAMPLE
// ============================================================================

/*
JavaScript Client:

```javascript
class LogStreamer {
    constructor(deploymentId, token) {
        this.deploymentId = deploymentId;
        this.token = token;
        this.ws = null;
        this.onLog = null;
        this.onError = null;
        this.onClose = null;
    }

    connect(options = {}) {
        const params = new URLSearchParams({
            token: this.token,
            tail: options.tail || 100,
            follow: options.follow !== false,
            streams: options.streams || 'stdout,stderr',
            filter: options.filter || '',
        });

        const url = `ws://localhost:8080/api/deployments/${this.deploymentId}/logs?${params}`;
        this.ws = new WebSocket(url);

        this.ws.onopen = () => {
            console.log('Log stream connected');
        };

        this.ws.onmessage = (event) => {
            const data = JSON.parse(event.data);
            if (data.line) {
                // Log message
                if (this.onLog) {
                    this.onLog(data);
                }
            } else if (data.message === 'log stream ended') {
                if (this.onClose) {
                    this.onClose();
                }
            } else if (data.error) {
                if (this.onError) {
                    this.onError(data.error);
                }
            }
        };

        this.ws.onerror = (error) => {
            console.error('WebSocket error:', error);
            if (this.onError) {
                this.onError(error);
            }
        };

        this.ws.onclose = () => {
            console.log('Log stream closed');
            if (this.onClose) {
                this.onClose();
            }
        };
    }

    disconnect() {
        if (this.ws) {
            this.ws.close();
        }
    }

    // Example: Filter to only error logs
    filterErrors() {
        const options = {
            tail: 100,
            follow: true,
            streams: 'stderr',
            filter: 'error',
        };
        this.connect(options);
    }
}

// Usage
const streamer = new LogStreamer('deployment-123', 'jwt-token');
streamer.onLog = (log) => {
    console.log(`[${log.stream}] ${log.line}`);
};
streamer.connect({ tail: 50 });