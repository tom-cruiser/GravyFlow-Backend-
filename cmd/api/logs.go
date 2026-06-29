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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var logsWebsocketUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type logStreamMessage struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

type websocketLineWriter struct {
	conn   *websocket.Conn
	stream string
	mu     *sync.Mutex
	buffer bytes.Buffer
}

func (w *websocketLineWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	written := len(p)
	for len(p) > 0 {
		newlineIndex := bytes.IndexByte(p, '\n')
		if newlineIndex < 0 {
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

	return w.conn.WriteJSON(logStreamMessage{
		Stream: w.stream,
		Line:   line,
	})
}

func streamAppLogsHandler(c *gin.Context) {
	// WebSocket connections cannot send Authorization headers, so we accept
	// the JWT as a ?token= query parameter and validate it manually.
	var user UserRecord
	if bearerToken := strings.TrimSpace(c.Query("token")); bearerToken != "" {
		claims, err := parseAndValidateToken(bearerToken, tokenTypeAccess)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
			return
		}
		u, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		user = u
	} else {
		var ok bool
		user, ok = currentAuthUser(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
	}

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
	containerID := strings.TrimSpace(deployment.ContainerID)
	if containerID == "" {
		// Upgrade to WS first so the client gets a readable message in the log viewer
		conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		_ = conn.WriteJSON(logStreamMessage{Stream: "stderr", Line: "[info] container not ready yet — deployment is still building."})
		conn.Close()
		return
	}

	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		_ = conn.WriteJSON(gin.H{"error": fmt.Sprintf("create docker client: %v", err)})
		return
	}
	defer dockerClient.Close()

	logStream, err := dockerClient.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "all",
	})
	if err != nil {
		_ = conn.WriteJSON(gin.H{"error": fmt.Sprintf("open container logs: %v", err)})
		return
	}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			cancel()
			_ = logStream.Close()
		})
	}
	defer cleanup()

	writerMu := &sync.Mutex{}
	stdoutWriter := &websocketLineWriter{conn: conn, stream: "stdout", mu: writerMu}
	stderrWriter := &websocketLineWriter{conn: conn, stream: "stderr", mu: writerMu}
	streamDone := make(chan struct{})
	readErr := make(chan error, 1)

	go func() {
		defer close(streamDone)
		defer cleanup()

		_, streamErr := stdcopy.StdCopy(stdoutWriter, stderrWriter, logStream)
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, io.EOF) {
			readErr <- streamErr
			return
		}

		if flushErr := stdoutWriter.flushRemaining(); flushErr != nil {
			readErr <- flushErr
			return
		}
		if flushErr := stderrWriter.flushRemaining(); flushErr != nil {
			readErr <- flushErr
			return
		}
	}()

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cleanup()
				return
			}
		}
	}()

	select {
	case err := <-readErr:
		if err != nil {
			_ = conn.WriteJSON(gin.H{"error": err.Error()})
		}
	case <-streamDone:
	case <-ctx.Done():
	}
}
