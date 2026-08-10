package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	maxLogLines     = 10000
	maxLogSize      = 10 * 1024 * 1024 // 10MB
	defaultLogLimit = 100
)

// ============================================================================
// TYPES
// ============================================================================

type LogLevel string

const (
	LogLevelInfo    LogLevel = "info"
	LogLevelWarning LogLevel = "warning"
	LogLevelError   LogLevel = "error"
	LogLevelDebug   LogLevel = "debug"
)

type deployLogLine struct {
	Text       string    `json:"text"`
	Stream     string    `json:"stream"`
	Level      LogLevel  `json:"level,omitempty"`
	Time       time.Time `json:"time,omitempty"`
	Source     string    `json:"source,omitempty"`
	LineNumber int       `json:"lineNumber,omitempty"`
}

type LogFilter struct {
	Level   string `form:"level"`
	Search  string `form:"search"`
	Since   string `form:"since"`
	Until   string `form:"until"`
	Limit   int    `form:"limit"`
	Offset  int    `form:"offset"`
	Source  string `form:"source"`
	Stream  string `form:"stream"`
}

type LogStats struct {
	TotalLines   int            `json:"totalLines"`
	ErrorCount   int            `json:"errorCount"`
	WarningCount int            `json:"warningCount"`
	InfoCount    int            `json:"infoCount"`
	DebugCount   int            `json:"debugCount"`
	StartTime    time.Time      `json:"startTime"`
	EndTime      time.Time      `json:"endTime"`
	Duration     time.Duration  `json:"duration"`
	UniqueErrors []string       `json:"uniqueErrors,omitempty"`
	Sources      []string       `json:"sources,omitempty"`
}

type LogResponse struct {
	DeploymentID  string           `json:"deploymentId"`
	Status        string           `json:"status"`
	StatusMessage string           `json:"statusMessage"`
	Lines         []deployLogLine  `json:"lines"`
	Total         int              `json:"total"`
	Filter        LogFilter        `json:"filter"`
	Timestamp     time.Time        `json:"timestamp"`
	Job           interface{}      `json:"job,omitempty"`
	HasMore       bool             `json:"hasMore"`
}

// ============================================================================
// MAIN HANDLERS
// ============================================================================

// deployLogHandler returns deployment logs with optional filtering
func deployLogHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var filter LogFilter
	if err := c.ShouldBindQuery(&filter); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filter parameters"})
		return
	}

	// Validate pagination parameters before they're used in slice operations.
	if filter.Offset < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be >= 0"})
		return
	}
	if filter.Limit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be >= 0"})
		return
	}

	// Set defaults
	if filter.Limit == 0 {
		filter.Limit = defaultLogLimit
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}

	// Get enhanced logs
	lines := getEnhancedLogs(deployment, filter)

	// Calculate total before truncation
	total := len(lines)

	// Truncate if needed
	if len(lines) > maxLogLines {
		lines = truncateLogs(lines)
	}

	// Apply pagination, clamping to the available range so an offset beyond
	// the end of the log simply yields an empty page instead of panicking.
	start := filter.Offset
	if start > len(lines) {
		start = len(lines)
	}
	end := start + filter.Limit
	if end > len(lines) {
		end = len(lines)
	}

	paginatedLines := lines[start:end]
	hasMore := end < len(lines)

	response := LogResponse{
		DeploymentID:  deployment.DeploymentID,
		Status:        deployment.Status,
		StatusMessage: deployment.StatusMessage,
		Lines:         paginatedLines,
		Total:         total,
		Filter:        filter,
		Timestamp:     time.Now(),
		HasMore:       hasMore,
	}

	// Add job status if present
	jobID := strings.TrimSpace(c.Query("jobId"))
	if jobID != "" && deploymentJobs != nil {
		if jobStatus, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID); err == nil && found && jobStatus.UserID == user.ID {
			response.Job = jobStatus
			
			// Append job logs if not already included
			if strings.TrimSpace(jobStatus.Error) != "" {
				jobLines := formatDeployLogLinesEnhanced(jobStatus.Error, "job")
				response.Lines = appendUniqueDeployLinesEnhanced(response.Lines, jobLines)
			}
			if strings.TrimSpace(jobStatus.Message) != "" {
				jobLines := formatDeployLogLinesEnhanced(jobStatus.Message, "job")
				response.Lines = appendUniqueDeployLinesEnhanced(response.Lines, jobLines)
			}
		}
	}

	c.JSON(http.StatusOK, response)
}

// deployLogStreamHandler handles Server-Sent Events for real-time logs
func deployLogStreamHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("X-Accel-Buffering", "no") // Disable nginx buffering

	ctx := c.Request.Context()
	
	// Get initial state
	lastLineCount := 0
	lastStatus := ""
	
	// Send initial state
	initialLines := formatDeployLogLinesEnhanced(deployment.StatusMessage, "deployment")
	lastLineCount = len(initialLines)
	lastStatus = deployment.Status
	
	if err := streamEvent(c, "init", map[string]interface{}{
		"deploymentId":  deployment.DeploymentID,
		"status":        deployment.Status,
		"statusMessage": deployment.StatusMessage,
		"lines":         initialLines,
		"lineCount":     len(initialLines),
	}); err != nil {
		return
	}

	// Start polling for updates
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			streamEvent(c, "close", map[string]interface{}{
				"reason": "client disconnected",
			})
			return

		case <-ticker.C:
			// Reload deployment status
			updatedDeployment, err := deploymentStore.GetDeploymentForUser(ctx, user.ID, deployment.DeploymentID)
			if err != nil {
				continue
			}

			// Get current logs
			currentLines := formatDeployLogLinesEnhanced(updatedDeployment.StatusMessage, "deployment")
			
			// Check for changes
			statusChanged := updatedDeployment.Status != lastStatus
			linesChanged := len(currentLines) != lastLineCount

			if statusChanged || linesChanged {
				data := map[string]interface{}{
					"status":        updatedDeployment.Status,
					"statusMessage": updatedDeployment.StatusMessage,
					"lineCount":     len(currentLines),
				}

				// Send only new lines if lines changed
				if linesChanged && len(currentLines) > lastLineCount {
					newLines := currentLines[lastLineCount:]
					data["newLines"] = newLines
					data["lines"] = currentLines
				}

				// If status changed, send full state
				if statusChanged {
					data["lines"] = currentLines
					data["statusChanged"] = true
				}

				if err := streamEvent(c, "update", data); err != nil {
					return
				}

				lastLineCount = len(currentLines)
				lastStatus = updatedDeployment.Status
			}

			// Send heartbeat to keep connection alive
			if err := streamEvent(c, "ping", map[string]interface{}{
				"timestamp": time.Now(),
			}); err != nil {
				return
			}
		}
	}
}

// deployLogDownloadHandler allows downloading logs in different formats
func deployLogDownloadHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	format := c.DefaultQuery("format", "txt")
	includeJob := c.DefaultQuery("includeJob", "true") == "true"
	
	lines := formatDeployLogLinesEnhanced(deployment.StatusMessage, "deployment")
	
	// Include job logs if requested
	if includeJob {
		jobID := strings.TrimSpace(c.Query("jobId"))
		if jobID != "" && deploymentJobs != nil {
			if jobStatus, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID); err == nil && found && jobStatus.UserID == user.ID {
				if strings.TrimSpace(jobStatus.Error) != "" {
					jobLines := formatDeployLogLinesEnhanced(jobStatus.Error, "job")
					lines = appendUniqueDeployLinesEnhanced(lines, jobLines)
				}
				if strings.TrimSpace(jobStatus.Message) != "" {
					jobLines := formatDeployLogLinesEnhanced(jobStatus.Message, "job")
					lines = appendUniqueDeployLinesEnhanced(lines, jobLines)
				}
			}
		}
	}
	
	exporter := &LogExporter{}
	var data []byte
	var contentType string
	var filename string
	
	baseFilename := fmt.Sprintf("deployment-%s-%s", 
		strings.ReplaceAll(deployment.AppName, "/", "-"), 
		time.Now().Format("20060102-150405"))
	
	switch format {
	case "json":
		data, _ = exporter.ExportJSON(lines)
		contentType = "application/json"
		filename = baseFilename + ".json"
	case "csv":
		data, _ = exporter.ExportCSV(lines)
		contentType = "text/csv"
		filename = baseFilename + ".csv"
	case "html":
		data, _ = exporter.ExportHTML(lines)
		contentType = "text/html"
		filename = baseFilename + ".html"
	default: // txt
		var logContent strings.Builder
		logContent.WriteString(fmt.Sprintf("Deployment Logs - %s\n", deployment.AppName))
		logContent.WriteString(fmt.Sprintf("Deployment ID: %s\n", deployment.DeploymentID))
		logContent.WriteString(fmt.Sprintf("Status: %s\n", deployment.Status))
		logContent.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))
		logContent.WriteString(strings.Repeat("-", 80) + "\n\n")
		
		for _, line := range lines {
			logContent.WriteString(fmt.Sprintf("[%s] [%s] %s\n", 
				line.Time.Format("15:04:05"), 
				strings.ToUpper(string(line.Level)),
				line.Text))
		}
		data = []byte(logContent.String())
		contentType = "text/plain"
		filename = baseFilename + ".log"
	}
	
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", fmt.Sprintf("%d", len(data)))
	c.Data(http.StatusOK, contentType, data)
}

// deployLogStatsHandler returns statistics about deployment logs
func deployLogStatsHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	lines := formatDeployLogLinesEnhanced(deployment.StatusMessage, "deployment")
	
	// Include job logs if requested
	jobID := strings.TrimSpace(c.Query("jobId"))
	if jobID != "" && deploymentJobs != nil {
		if jobStatus, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID); err == nil && found && jobStatus.UserID == user.ID {
			if strings.TrimSpace(jobStatus.Error) != "" {
				jobLines := formatDeployLogLinesEnhanced(jobStatus.Error, "job")
				lines = appendUniqueDeployLinesEnhanced(lines, jobLines)
			}
			if strings.TrimSpace(jobStatus.Message) != "" {
				jobLines := formatDeployLogLinesEnhanced(jobStatus.Message, "job")
				lines = appendUniqueDeployLinesEnhanced(lines, jobLines)
			}
		}
	}
	
	stats := calculateLogStats(lines)
	
	// Add additional info
	stats.Sources = getUniqueSources(lines)
	
	c.JSON(http.StatusOK, stats)
}

// deployLogTailHandler returns only the last N lines of logs
func deployLogTailHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	tail := 100
	if t := c.Query("tail"); t != "" {
		parsedTail := 0
		parsed, err := fmt.Sscanf(t, "%d", &parsedTail)
		if err != nil || parsed != 1 || parsedTail < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tail must be a non-negative integer"})
			return
		}
		tail = parsedTail
		if tail > 10000 {
			tail = 10000
		}
	}

	lines := formatDeployLogLinesEnhanced(deployment.StatusMessage, "deployment")
	
	// Include job logs if present
	jobID := strings.TrimSpace(c.Query("jobId"))
	if jobID != "" && deploymentJobs != nil {
		if jobStatus, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID); err == nil && found && jobStatus.UserID == user.ID {
			if strings.TrimSpace(jobStatus.Error) != "" {
				jobLines := formatDeployLogLinesEnhanced(jobStatus.Error, "job")
				lines = appendUniqueDeployLinesEnhanced(lines, jobLines)
			}
			if strings.TrimSpace(jobStatus.Message) != "" {
				jobLines := formatDeployLogLinesEnhanced(jobStatus.Message, "job")
				lines = appendUniqueDeployLinesEnhanced(lines, jobLines)
			}
		}
	}
	
	// Get last N lines
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	c.JSON(http.StatusOK, gin.H{
		"deploymentId":  deployment.DeploymentID,
		"status":        deployment.Status,
		"statusMessage": deployment.StatusMessage,
		"lines":         lines,
		"count":         len(lines),
		"timestamp":     time.Now(),
	})
}

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

func getEnhancedLogs(deployment DeploymentRecord, filter LogFilter) []deployLogLine {
	// Get base logs
	lines := formatDeployLogLinesEnhanced(deployment.StatusMessage, "deployment")
	
	// Apply filters
	var since, until time.Time
	if filter.Since != "" {
		if t, err := time.Parse(time.RFC3339, filter.Since); err == nil {
			since = t
		} else if t, err := time.Parse("2006-01-02", filter.Since); err == nil {
			since = t
		}
	}
	if filter.Until != "" {
		if t, err := time.Parse(time.RFC3339, filter.Until); err == nil {
			until = t
		} else if t, err := time.Parse("2006-01-02", filter.Until); err == nil {
			until = t.Add(24 * time.Hour)
		}
	}
	
	filtered := make([]deployLogLine, 0, len(lines))
	for _, line := range lines {
		// Filter by level
		if filter.Level != "" && strings.ToLower(string(line.Level)) != strings.ToLower(filter.Level) {
			continue
		}
		
		// Filter by search term
		if filter.Search != "" && !strings.Contains(strings.ToLower(line.Text), strings.ToLower(filter.Search)) {
			continue
		}
		
		// Filter by source
		if filter.Source != "" && !strings.Contains(strings.ToLower(line.Source), strings.ToLower(filter.Source)) {
			continue
		}
		
		// Filter by stream
		if filter.Stream != "" && strings.ToLower(line.Stream) != strings.ToLower(filter.Stream) {
			continue
		}
		
		// Filter by time range
		if !since.IsZero() && line.Time.Before(since) {
			continue
		}
		if !until.IsZero() && line.Time.After(until) {
			continue
		}
		
		filtered = append(filtered, line)
	}
	
	return filtered
}

func formatDeployLogLinesEnhanced(message string, source string) []deployLogLine {
	message = strings.TrimSpace(message)
	if message == "" {
		return []deployLogLine{}
	}

	parts := strings.Split(message, "\n")
	out := make([]deployLogLine, 0, len(parts))
	now := time.Now()
	
	for i, line := range parts {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		
		parsedTime, parsedLine := parseTimestampFromLine(line)
		if parsedTime.IsZero() {
			parsedTime = now
		}
		
		// Detect stream type
		stream := "stderr"
		if strings.HasPrefix(strings.ToLower(line), "stdout:") {
			stream = "stdout"
			line = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(strings.ToLower(line), "stderr:") {
			stream = "stderr"
			line = strings.TrimSpace(line[7:])
		}
		
		out = append(out, deployLogLine{
			Text:       parsedLine,
			Stream:     stream,
			Level:      detectLogLevel(parsedLine),
			Time:       parsedTime,
			Source:     source,
			LineNumber: i + 1,
		})
	}
	return out
}

func detectLogLevel(line string) LogLevel {
	lineLower := strings.ToLower(line)
	
	// Error patterns
	if strings.Contains(lineLower, "error") || 
	   strings.Contains(lineLower, "failed") ||
	   strings.Contains(lineLower, "fatal") ||
	   strings.Contains(lineLower, "exception") ||
	   strings.Contains(lineLower, "panic") ||
	   strings.Contains(lineLower, "critical") {
		return LogLevelError
	}
	
	// Warning patterns
	if strings.Contains(lineLower, "warn") || 
	   strings.Contains(lineLower, "warning") ||
	   strings.Contains(lineLower, "deprecated") ||
	   strings.Contains(lineLower, "skipping") {
		return LogLevelWarning
	}
	
	// Debug patterns
	if strings.Contains(lineLower, "debug") || 
	   strings.Contains(lineLower, "trace") ||
	   strings.Contains(lineLower, "verbose") ||
	   strings.Contains(lineLower, "detail") {
		return LogLevelDebug
	}
	
	// Default to info
	return LogLevelInfo
}

func parseTimestampFromLine(line string) (time.Time, string) {
	// Try common timestamp formats
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
		"15:04:05",
		"2006-01-02T15:04:05Z",
		"Jan _2 15:04:05",
		"Jan _2, 2006 15:04:05",
	}
	
	for _, format := range formats {
		if len(line) >= len(format) {
			if t, err := time.Parse(format, line[:len(format)]); err == nil {
				return t, strings.TrimSpace(line[len(format):])
			}
		}
	}
	
	// Try to find timestamp anywhere in the line
	for _, format := range formats {
		if idx := strings.Index(line, format[:10]); idx >= 0 {
			if len(line) >= idx+len(format) {
				if t, err := time.Parse(format, line[idx:idx+len(format)]); err == nil {
					remaining := strings.TrimSpace(line[:idx] + line[idx+len(format):])
					return t, remaining
				}
			}
		}
	}
	
	return time.Time{}, line
}

func appendUniqueDeployLinesEnhanced(base []deployLogLine, extra []deployLogLine) []deployLogLine {
	if len(extra) == 0 {
		return base
	}
	
	seen := make(map[string]struct{}, len(base))
	for _, line := range base {
		seen[line.Text] = struct{}{}
	}
	
	for _, line := range extra {
		if _, ok := seen[line.Text]; ok {
			continue
		}
		base = append(base, line)
		seen[line.Text] = struct{}{}
	}
	return base
}

func truncateLogs(lines []deployLogLine) []deployLogLine {
	if len(lines) <= maxLogLines {
		return lines
	}
	
	kept := make([]deployLogLine, 0, maxLogLines)
	
	// Keep first 1000 lines
	kept = append(kept, lines[:1000]...)
	
	// Add truncation marker
	kept = append(kept, deployLogLine{
		Text:   fmt.Sprintf("... [TRUNCATED] ... Showing last %d lines (total: %d)", 
			maxLogLines-1001, len(lines)),
		Stream: "stderr",
		Level:  LogLevelWarning,
		Time:   time.Now(),
		Source: "system",
	})
	
	// Add last 9000 lines
	kept = append(kept, lines[len(lines)-9000:]...)
	
	return kept
}

func calculateLogStats(lines []deployLogLine) LogStats {
	stats := LogStats{
		TotalLines: len(lines),
	}
	
	errorSet := make(map[string]bool)
	sourceSet := make(map[string]bool)
	
	for _, line := range lines {
		switch line.Level {
		case LogLevelError:
			stats.ErrorCount++
			if len(line.Text) > 0 {
				errorSet[line.Text] = true
			}
		case LogLevelWarning:
			stats.WarningCount++
		case LogLevelInfo:
			stats.InfoCount++
		case LogLevelDebug:
			stats.DebugCount++
		}
		
		if line.Source != "" {
			sourceSet[line.Source] = true
		}
		
		if stats.StartTime.IsZero() || line.Time.Before(stats.StartTime) {
			stats.StartTime = line.Time
		}
		if line.Time.After(stats.EndTime) {
			stats.EndTime = line.Time
		}
	}
	
	if !stats.StartTime.IsZero() && !stats.EndTime.IsZero() {
		stats.Duration = stats.EndTime.Sub(stats.StartTime)
	}
	
	// Get unique errors (max 20)
	for err := range errorSet {
		if len(stats.UniqueErrors) < 20 {
			stats.UniqueErrors = append(stats.UniqueErrors, err)
		}
	}
	
	// Get unique sources
	for source := range sourceSet {
		stats.Sources = append(stats.Sources, source)
	}
	
	return stats
}

func getUniqueSources(lines []deployLogLine) []string {
	sourceSet := make(map[string]bool)
	for _, line := range lines {
		if line.Source != "" {
			sourceSet[line.Source] = true
		}
	}
	
	sources := make([]string, 0, len(sourceSet))
	for source := range sourceSet {
		sources = append(sources, source)
	}
	return sources
}

func streamEvent(c *gin.Context, event string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	
	_, err = c.Writer.Write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, jsonData)))
	if err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

// ============================================================================
// LOG EXPORTER
// ============================================================================

type LogExporter struct{}

func (e *LogExporter) ExportJSON(lines []deployLogLine) ([]byte, error) {
	return json.MarshalIndent(lines, "", "  ")
}

func (e *LogExporter) ExportCSV(lines []deployLogLine) ([]byte, error) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	
	// Write header
	writer.Write([]string{"time", "level", "source", "stream", "text"})
	
	// Write rows
	for _, line := range lines {
		writer.Write([]string{
			line.Time.Format(time.RFC3339),
			string(line.Level),
			line.Source,
			line.Stream,
			line.Text,
		})
	}
	
	writer.Flush()
	return buf.Bytes(), nil
}

func (e *LogExporter) ExportHTML(lines []deployLogLine) ([]byte, error) {
	var buf bytes.Buffer
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Deployment Logs</title>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <style>
        * { box-sizing: border-box; }
        body { 
            font-family: 'Courier New', monospace; 
            background: #1e1e1e; 
            color: #d4d4d4; 
            padding: 0; 
            margin: 0;
            font-size: 13px;
            line-height: 1.5;
        }
        .controls { 
            background: #252525; 
            padding: 10px 20px; 
            position: sticky; 
            top: 0; 
            z-index: 100; 
            border-bottom: 2px solid #333;
            display: flex;
            gap: 15px;
            flex-wrap: wrap;
            align-items: center;
        }
        .controls .stat {
            color: #6a9955;
            margin-right: 5px;
            font-size: 12px;
        }
        .controls .stat.error { color: #f48771; }
        .controls .stat.warning { color: #dcdcaa; }
        .controls .stat.info { color: #9cdcfe; }
        .filter-input {
            background: #333;
            color: #fff;
            border: 1px solid #555;
            padding: 5px 10px;
            border-radius: 3px;
            font-size: 12px;
            font-family: inherit;
        }
        .filter-input:focus {
            outline: none;
            border-color: #007acc;
        }
        .logs-container {
            padding: 10px 20px;
        }
        .log-line { 
            padding: 2px 5px; 
            border-bottom: 1px solid #2d2d2d; 
            white-space: pre-wrap; 
            word-wrap: break-word;
            transition: background 0.2s;
        }
        .log-line:hover { background: #2d2d2d; }
        .log-line .error { color: #f48771; }
        .log-line .warning { color: #dcdcaa; }
        .log-line .info { color: #9cdcfe; }
        .log-line .debug { color: #808080; }
        .log-line .timestamp { 
            color: #6a9955; 
            margin-right: 10px;
            display: inline-block;
            min-width: 80px;
        }
        .log-line .source { 
            color: #c586c0; 
            margin-right: 10px;
            display: inline-block;
            min-width: 80px;
        }
        .log-line .level-badge {
            display: inline-block;
            padding: 0 6px;
            border-radius: 3px;
            font-size: 10px;
            font-weight: bold;
            margin-right: 8px;
            text-transform: uppercase;
        }
        .log-line .level-badge.error { 
            background: #f48771;
            color: #1e1e1e;
        }
        .log-line .level-badge.warning { 
            background: #dcdcaa;
            color: #1e1e1e;
        }
        .log-line .level-badge.info { 
            background: #9cdcfe;
            color: #1e1e1e;
        }
        .log-line .level-badge.debug { 
            background: #808080;
            color: #1e1e1e;
        }
        .log-line .text { 
            color: #d4d4d4;
        }
        .highlight { background: #2d2d2d; }
        @media (max-width: 768px) {
            .controls { padding: 10px; gap: 10px; }
            .controls .stat { font-size: 11px; }
            .filter-input { font-size: 11px; padding: 4px 8px; }
            .log-line { font-size: 12px; }
            .log-line .timestamp { min-width: 60px; }
            .log-line .source { min-width: 60px; }
        }
        @media print {
            .controls { display: none; }
            .log-line { border-bottom: none; }
            .log-line:hover { background: transparent; }
        }
        .empty-state {
            text-align: center;
            padding: 50px 20px;
            color: #808080;
        }
        .error-state {
            text-align: center;
            padding: 50px 20px;
            color: #f48771;
        }
    </style>
    <script>
        function filterLogs() {
            const search = document.getElementById('search').value.toLowerCase();
            const level = document.getElementById('level').value;
            const source = document.getElementById('source').value;
            const lines = document.querySelectorAll('.log-line');
            let visible = 0;
            lines.forEach(line => {
                const text = line.textContent.toLowerCase();
                const levelClass = line.className.includes('error') ? 'error' :
                                 line.className.includes('warning') ? 'warning' :
                                 line.className.includes('info') ? 'info' : 'debug';
                const sourceClass = line.className.includes('source-') ? 
                    line.className.split(' ').find(c => c.startsWith('source-')).replace('source-', '') : '';
                let show = true;
                if (search && !text.includes(search)) show = false;
                if (level && level !== 'all' && levelClass !== level) show = false;
                if (source && source !== 'all' && sourceClass !== source) show = false;
                line.style.display = show ? 'block' : 'none';
                if (show) visible++;
            });
            document.getElementById('visibleCount').textContent = visible;
        }
        function exportLogs(format) {
            const url = window.location.pathname + '/download?format=' + format;
            window.location.href = url;
        }
        function toggleHighlight() {
            const lines = document.querySelectorAll('.log-line');
            lines.forEach(line => line.classList.toggle('highlight'));
        }
        document.addEventListener('DOMContentLoaded', function() {
            document.getElementById('search').addEventListener('input', filterLogs);
            document.getElementById('level').addEventListener('change', filterLogs);
            document.getElementById('source').addEventListener('change', filterLogs);
        });
        window.exportLogs = exportLogs;
        window.toggleHighlight = toggleHighlight;
    </script>
</head>
<body>
    <div class="controls">
        <span class="stat">📊 Total: <span id="totalCount">%d</span> lines</span>
        <span class="stat">👁️ Visible: <span id="visibleCount">%d</span></span>
        <span class="stat error">❌ Errors: <span id="errorCount">%d</span></span>
        <span class="stat warning">⚠️ Warnings: <span id="warningCount">%d</span></span>
        <input id="search" class="filter-input" placeholder="🔍 Search logs..." type="text">
        <select id="level" class="filter-input">
            <option value="all">All Levels</option>
            <option value="error">Error</option>
            <option value="warning">Warning</option>
            <option value="info">Info</option>
            <option value="debug">Debug</option>
        </select>
        <select id="source" class="filter-input">
            <option value="all">All Sources</option>
            %s
        </select>
        <button onclick="exportLogs('txt')" class="filter-input">📥 TXT</button>
        <button onclick="exportLogs('json')" class="filter-input">📥 JSON</button>
        <button onclick="exportLogs('csv')" class="filter-input">📥 CSV</button>
        <button onclick="exportLogs('html')" class="filter-input">📥 HTML</button>
        <button onclick="toggleHighlight()" class="filter-input">🎨 Highlight</button>
    </div>
    <div class="logs-container">`
	
	// Calculate stats for header
	errorCount, warningCount := 0, 0
	sourceSet := make(map[string]bool)
	for _, line := range lines {
		if line.Level == LogLevelError {
			errorCount++
		} else if line.Level == LogLevelWarning {
			warningCount++
		}
		if line.Source != "" {
			sourceSet[line.Source] = true
		}
	}
	
	// Build source options
	sourceOptions := ""
	for source := range sourceSet {
		sourceOptions += fmt.Sprintf("<option value=\"%s\">%s</option>", source, source)
	}
	
	buf.WriteString(fmt.Sprintf(tmpl, len(lines), len(lines), errorCount, warningCount, sourceOptions))
	
	if len(lines) == 0 {
		buf.WriteString(`<div class="empty-state">No logs available</div>`)
	} else {
		for _, line := range lines {
			levelClass := strings.ToLower(string(line.Level))
			sourceClass := "source-" + strings.ReplaceAll(line.Source, " ", "-")
			buf.WriteString(fmt.Sprintf(
				`<div class="log-line %s %s">
					<span class="timestamp">%s</span>
					<span class="level-badge %s">%s</span>
					<span class="source">%s</span>
					<span class="text">%s</span>
				</div>`,
				levelClass,
				sourceClass,
				line.Time.Format("15:04:05"),
				levelClass,
				strings.ToUpper(string(line.Level)),
				line.Source,
				line.Text,
			))
		}
	}
	
	buf.WriteString(`</div></body></html>`)
	return buf.Bytes(), nil
}

// ============================================================================
// ROUTE SETUP
// ============================================================================

// SetupDeployLogRoutes configures all deployment log routes
func SetupDeployLogRoutes(router *gin.Engine) {
	deployGroup := router.Group("/api/deployments/:id")
	{
		// Protected routes (require authentication)
		deployGroup.Use(AuthMiddleware(false))
		
		// Main log endpoints
		deployGroup.GET("/logs", deployLogHandler)
		deployGroup.GET("/logs/stream", deployLogStreamHandler)
		deployGroup.GET("/logs/download", deployLogDownloadHandler)
		deployGroup.GET("/logs/stats", deployLogStatsHandler)
		deployGroup.GET("/logs/tail", deployLogTailHandler)
		
		// WebSocket support (alternative to SSE)
		deployGroup.GET("/logs/ws", deployLogWebSocketHandler)
	}
}

// ============================================================================
// WEBSOCKET SUPPORT (Optional)
// ============================================================================

// deployLogWebSocketHandler handles WebSocket connections for real-time logs
func deployLogWebSocketHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	// Note: This requires a WebSocket library like gorilla/websocket
	// This is a placeholder for WebSocket implementation
	
	c.JSON(http.StatusOK, gin.H{
		"message": "WebSocket support coming soon",
		"deploymentId": deployment.DeploymentID,
		"userId": user.ID,
	})
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
USAGE EXAMPLES:

1. Get all logs with default pagination:
   GET /api/deployments/123/logs

2. Get logs with filtering:
   GET /api/deployments/123/logs?level=error&search=failed&limit=50

3. Get logs with time range:
   GET /api/deployments/123/logs?since=2024-01-01&until=2024-01-31

4. Stream real-time logs (SSE):
   GET /api/deployments/123/logs/stream

5. Download logs:
   GET /api/deployments/123/logs/download?format=json
   GET /api/deployments/123/logs/download?format=csv
   GET /api/deployments/123/logs/download?format=html

6. Get log statistics:
   GET /api/deployments/123/logs/stats

7. Get last N lines:
   GET /api/deployments/123/logs/tail?tail=50

8. Include job logs:
   GET /api/deployments/123/logs?jobId=job-456
*/