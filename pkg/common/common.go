// Package common provides small shared helpers used across the client and CLI
// packages to avoid duplicate implementations.
package common

// Truncate shortens s to at most maxLen bytes, appending "..." if it was
// truncated. Safe for logging previews of long strings.
func Truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
