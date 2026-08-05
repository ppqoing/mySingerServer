package app

import (
	"regexp"

	"dedup/internal/nodectl"
)

var (
	uiSecretAssignment = regexp.MustCompile(`(?i)\b(?:password|pgpassword|dsn|token)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s;,]+)`)
	uiWindowsPath      = regexp.MustCompile(`(?i)(?:\b[a-z]:\\|\\\\)[^\s]+`)
)

// sanitizeText is the single boundary for all strings that can reach Wails,
// a tray notification, an event, or an OperationResult.
func sanitizeText(value string) string {
	value = nodectl.SanitizeSummary(value)
	value = uiSecretAssignment.ReplaceAllString(value, "[REDACTED]")
	value = uiWindowsPath.ReplaceAllString(value, "[REDACTED_PATH]")
	return nodectl.SanitizeSummary(value)
}
