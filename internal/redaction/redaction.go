package redaction

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	diagnosticURLPattern         = regexp.MustCompile(`(?i)\b(?:https?|rtmp|rtmps|srt|rtsp|tcp|udp)://[^\s"'<>]+`)
	diagnosticCredentialPattern  = regexp.MustCompile(`(?i)(?:authorization\s*[:=]\s*(?:bearer\s+)?|bearer\s+|(?:token|secret|password|passphrase)\s*[:=]\s*)[^\s,;]+`)
	diagnosticIngestTokenPattern = regexp.MustCompile(`(?i)\bast_ingest_v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
)

func Args(args []string, sensitiveValues ...string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		out[i] = redactSensitiveValues(out[i], sensitiveValues)
	}
	return out
}

func Message(message string, sensitiveValues ...string) string {
	return redactSensitiveValues(message, sensitiveValues)
}

// Diagnostic additionally masks arbitrary URLs and common credential-shaped
// values that may be emitted by an external process.  It is intended for
// bounded process diagnostics, where the complete set of values printed by
// the child process is not known in advance.
func Diagnostic(message string, sensitiveValues ...string) string {
	out := Message(message, sensitiveValues...)
	out = diagnosticCredentialPattern.ReplaceAllString(out, "<REDACTED>")
	out = diagnosticIngestTokenPattern.ReplaceAllString(out, "<REDACTED>")
	out = diagnosticURLPattern.ReplaceAllStringFunc(out, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return "<REDACTED_URL>"
		}
		return parsed.Scheme + "://" + parsed.Host + "/<REDACTED>"
	})
	return out
}

func redactSensitiveValues(input string, sensitiveValues []string) string {
	out := input
	for _, value := range sensitiveValues {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if masked, ok := MaskSensitiveURL(value); ok {
			out = strings.ReplaceAll(out, value, masked)
		}
	}
	for _, value := range sensitiveValues {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if looksLikeURL(value) {
			continue
		}
		out = strings.ReplaceAll(out, value, "<REDACTED>")
	}
	return out
}

func MaskSensitiveURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw, false
	}
	return parsed.Scheme + "://" + parsed.Host + "/<REDACTED>", true
}

func looksLikeURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}
