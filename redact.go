package main

import (
	"regexp"
)

var (
	qoderTokenRegex     = regexp.MustCompile(`sk-[a-zA-Z0-9_-]{32,}`)
	anthropicTokenRegex = regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]+`)
)

// RedactSensitiveInfo removes potential API tokens from a given string.
func RedactSensitiveInfo(text string) string {
	text = qoderTokenRegex.ReplaceAllString(text, "[REDACTED_TOKEN]")
	text = anthropicTokenRegex.ReplaceAllString(text, "[REDACTED_ANTHROPIC_TOKEN]")
	return text
}
