package main

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type deployLogLine struct {
	Text   string `json:"text"`
	Stream string `json:"stream"`
}

func deployLogHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	lines := formatDeployLogLines(deployment.StatusMessage)
	response := gin.H{
		"deploymentId":  deployment.DeploymentID,
		"status":        deployment.Status,
		"statusMessage": deployment.StatusMessage,
		"lines":         lines,
	}

	jobID := strings.TrimSpace(c.Query("jobId"))
	if jobID != "" && deploymentJobs != nil {
		if jobStatus, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID); err == nil && found && jobStatus.UserID == user.ID {
			response["job"] = jobStatus
			if strings.TrimSpace(jobStatus.Error) != "" {
				lines = appendUniqueDeployLines(lines, formatDeployLogLines(jobStatus.Error))
			}
			if strings.TrimSpace(jobStatus.Message) != "" {
				lines = appendUniqueDeployLines(lines, formatDeployLogLines(jobStatus.Message))
			}
			response["lines"] = lines
		}
	}

	c.JSON(http.StatusOK, response)
}

func formatDeployLogLines(message string) []deployLogLine {
	message = strings.TrimSpace(message)
	if message == "" {
		return []deployLogLine{}
	}

	parts := strings.Split(message, "\n")
	out := make([]deployLogLine, 0, len(parts))
	for _, line := range parts {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, deployLogLine{Text: line, Stream: "stderr"})
	}
	return out
}

func appendUniqueDeployLines(base []deployLogLine, extra []deployLogLine) []deployLogLine {
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
