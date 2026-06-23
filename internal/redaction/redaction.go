package redaction

import (
	"net/url"
	"strings"
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
