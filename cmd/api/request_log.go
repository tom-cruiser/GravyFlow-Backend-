package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// sensitiveQueryParams are query-string keys whose values must never reach
// the request log. The log WebSocket authenticates with ?token=<JWT> (browsers
// can't set headers on a WebSocket), and the GitHub OAuth callback carries
// one-time code/state values; logging them leaves live credentials in
// `docker logs`.
var sensitiveQueryParams = map[string]bool{
	"token":        true,
	"access_token": true,
	"code":         true,
	"state":        true,
}

// redactQuery returns path?query with sensitive parameter values replaced by
// "REDACTED". Parameter order and non-sensitive values are preserved.
func redactQuery(path string) string {
	i := strings.IndexByte(path, '?')
	if i < 0 {
		return path
	}
	parts := strings.Split(path[i+1:], "&")
	for j, part := range parts {
		key, _, hasValue := strings.Cut(part, "=")
		if !hasValue {
			continue
		}
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		if sensitiveQueryParams[strings.ToLower(decoded)] {
			parts[j] = key + "=REDACTED"
		}
	}
	return path[:i+1] + strings.Join(parts, "&")
}

// redactingLogFormatter matches gin's default log line format, minus colour,
// with sensitive query values redacted.
func redactingLogFormatter(p gin.LogFormatterParams) string {
	if p.Latency > time.Minute {
		p.Latency = p.Latency.Truncate(time.Second)
	}
	return fmt.Sprintf("[GIN] %v | %3d | %13v | %15s | %-7s %#v\n%s",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"),
		p.StatusCode,
		p.Latency,
		p.ClientIP,
		p.Method,
		redactQuery(p.Path),
		p.ErrorMessage,
	)
}
