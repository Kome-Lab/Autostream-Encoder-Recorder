package workerevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

type Event struct {
	ID         string         `json:"id"`
	StreamID   string         `json:"stream_id"`
	ServiceID  string         `json:"service_id,omitempty"`
	Generation uint64         `json:"job_generation,omitempty"`
	Attempt    uint32         `json:"attempt,omitempty"`
	Type       string         `json:"type"`
	Payload    map[string]any `json:"payload"`
	Timestamp  time.Time      `json:"timestamp"`
}

type TranscriptEntry struct {
	EventID            string    `json:"event_id"`
	UtteranceID        string    `json:"utterance_id,omitempty"`
	Revision           int       `json:"revision,omitempty"`
	Timestamp          time.Time `json:"timestamp"`
	StartedAt          time.Time `json:"started_at,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
	EndedAt            time.Time `json:"ended_at,omitempty"`
	Text               string    `json:"text"`
	SpeakerUserID      string    `json:"speaker_user_id,omitempty"`
	SpeakerDisplayName string    `json:"speaker_display_name,omitempty"`
	Confidence         float64   `json:"confidence,omitempty"`
	FinalizationReason string    `json:"finalization_reason,omitempty"`
	Source             string    `json:"source,omitempty"`
	Final              bool      `json:"is_final"`
}

type Result struct {
	Accepted           bool   `json:"accepted"`
	Duplicate          bool   `json:"duplicate,omitempty"`
	StreamID           string `json:"stream_id"`
	EventID            string `json:"event_id"`
	EventType          string `json:"event_type"`
	LogsArtifact       string `json:"logs_artifact,omitempty"`
	CaptionsArtifact   string `json:"captions_artifact,omitempty"`
	TranscriptArtifact string `json:"transcript_artifact,omitempty"`
}

type Manager struct {
	ArchiveRoot      string
	mu               sync.Mutex
	recent           map[string][]Event
	seen             map[string]time.Time
	maxRecent        int
	maxSeen          int
	latestGeneration map[string]uint64
}

func NewManager(archiveRoot string) *Manager {
	if archiveRoot == "" {
		archiveRoot = "/var/lib/autostream/archives"
	}
	return &Manager{ArchiveRoot: archiveRoot, recent: map[string][]Event{}, seen: map[string]time.Time{}, latestGeneration: map[string]uint64{}, maxRecent: 200, maxSeen: 2000}
}

func (m *Manager) Add(event Event) (Result, error) {
	if strings.TrimSpace(event.StreamID) == "" {
		return Result{}, errors.New("stream_id is required")
	}
	if strings.TrimSpace(event.Type) == "" {
		return Result{}, errors.New("event type is required")
	}
	if strings.TrimSpace(event.ID) == "" {
		return Result{}, errors.New("event id is required")
	}
	if !strings.HasPrefix(event.Type, "overlay.") && !strings.HasPrefix(event.Type, "caption.") {
		return Result{}, errors.New("event type must start with overlay. or caption.")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	event.Payload = safePayload(event.Payload)
	layout, err := archive.NewLayout(m.ArchiveRoot, event.StreamID)
	if err != nil {
		return Result{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Result{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Generation > 0 {
		if latest := m.latestGeneration[event.StreamID]; latest > 0 && event.Generation < latest {
			return Result{}, errors.New("stale worker event generation")
		}
		if event.Generation > m.latestGeneration[event.StreamID] {
			m.latestGeneration[event.StreamID] = event.Generation
		}
	}

	if key := eventDedupeKey(event); key != "" {
		if _, ok := m.seen[key]; ok {
			return Result{Accepted: true, Duplicate: true, StreamID: event.StreamID, EventID: event.ID, EventType: event.Type}, nil
		}
		m.seen[key] = event.Timestamp
		m.trimSeenLocked()
	}

	if err := appendJSONL(layout.TmpLogs(), map[string]any{
		"timestamp": event.Timestamp.Format(time.RFC3339Nano),
		"event":     "worker.event.received",
		"stream_id": event.StreamID,
		"event_id":  event.ID,
		"attempt":   event.Attempt,
		"type":      event.Type,
		"payload":   event.Payload,
	}); err != nil {
		return Result{}, err
	}
	result := Result{Accepted: true, StreamID: event.StreamID, EventID: event.ID, EventType: event.Type, LogsArtifact: "logs.jsonl"}
	if event.Type == "caption.final" {
		if err := m.appendCaption(layout, event); err != nil {
			return Result{}, err
		}
		result.CaptionsArtifact = "captions.vtt"
		result.TranscriptArtifact = "transcript.json"
	}
	m.remember(event)
	return result, nil
}

func (m *Manager) Recent(streamID string) ([]Event, error) {
	if strings.TrimSpace(streamID) == "" {
		return nil, errors.New("stream_id is required")
	}
	if _, err := archive.NewLayout(m.ArchiveRoot, streamID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Event(nil), m.recent[streamID]...)
	return out, nil
}

func (m *Manager) appendCaption(layout archive.Layout, event Event) error {
	text, _ := event.Payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("caption text is required")
	}
	if err := ensureVTT(layout.TmpCaptions()); err != nil {
		return err
	}
	start := event.Timestamp.UTC()
	end := start.Add(4 * time.Second)
	if raw, ok := event.Payload["started_at"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
			start = parsed.UTC()
		}
	}
	if raw, ok := event.Payload["ended_at"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
			end = parsed.UTC()
		}
	}
	if !end.After(start) {
		end = start.Add(4 * time.Second)
	}
	if end.Sub(start) > 10*time.Minute {
		end = start.Add(10 * time.Minute)
	}
	speakerName := firstPayloadString(event.Payload, "speaker_display_name", "display_name")
	cueText := text
	if speakerName != "" {
		cueText = speakerName + ": " + text
	}
	cue := fmt.Sprintf("\n%s --> %s\n%s\n", formatVTTTime(start), formatVTTTime(end), sanitizeCaptionText(cueText))
	if err := appendFile(layout.TmpCaptions(), []byte(cue)); err != nil {
		return err
	}
	entry := TranscriptEntry{
		EventID:            event.ID,
		UtteranceID:        firstPayloadString(event.Payload, "utterance_id"),
		Revision:           payloadInt(event.Payload, "revision"),
		Timestamp:          event.Timestamp.UTC(),
		StartedAt:          start,
		UpdatedAt:          payloadTime(event.Payload, "updated_at", event.Timestamp),
		EndedAt:            end,
		Text:               text,
		SpeakerUserID:      firstPayloadString(event.Payload, "speaker_user_id"),
		SpeakerDisplayName: speakerName,
		Confidence:         payloadFloat(event.Payload, "confidence"),
		FinalizationReason: firstPayloadString(event.Payload, "finalization_reason"),
		Source:             firstPayloadString(event.Payload, "source"),
		Final:              true,
	}
	return appendTranscript(layout.TmpTranscript(), entry)
}

func firstPayloadString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func payloadInt(payload map[string]any, key string) int {
	switch value := payload[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return 0
}

func payloadFloat(payload map[string]any, key string) float64 {
	value, _ := payload[key].(float64)
	return value
}

func payloadTime(payload map[string]any, key string, fallback time.Time) time.Time {
	if raw, ok := payload[key].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
			return parsed.UTC()
		}
	}
	return fallback.UTC()
}

func (m *Manager) remember(event Event) {
	m.recent[event.StreamID] = append(m.recent[event.StreamID], event)
	if len(m.recent[event.StreamID]) > m.maxRecent {
		m.recent[event.StreamID] = append([]Event(nil), m.recent[event.StreamID][len(m.recent[event.StreamID])-m.maxRecent:]...)
	}
}

func eventDedupeKey(event Event) string {
	if event.Type == "caption.final" {
		if utteranceID := firstPayloadString(event.Payload, "utterance_id"); utteranceID != "" {
			return event.StreamID + "\x00caption.final\x00" + utteranceID
		}
	}
	id := strings.TrimSpace(event.ID)
	if id == "" {
		return ""
	}
	return event.StreamID + "\x00" + id
}

func (m *Manager) trimSeenLocked() {
	limit := m.maxSeen
	if limit <= 0 {
		limit = 2000
	}
	if len(m.seen) <= limit {
		return
	}
	type seenEntry struct {
		key string
		at  time.Time
	}
	entries := make([]seenEntry, 0, len(m.seen))
	for key, at := range m.seen {
		entries = append(entries, seenEntry{key: key, at: at})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].at.Before(entries[j].at)
	})
	for _, entry := range entries[:len(entries)-limit] {
		delete(m.seen, entry.key)
	}
}

func appendJSONL(path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return appendFile(path, append(body, '\n'))
}

func appendFile(path string, body []byte) error {
	root, err := archive.RootFromSidecarPath(path)
	if err != nil {
		return err
	}
	if err := archive.EnsureDirNoSymlinks(root, filepath.Dir(path)); err != nil {
		return err
	}
	file, err := openAppendNoSymlink(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(body)
	return err
}

func ensureVTT(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("archive sidecar path is not a regular file")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeFileNoSymlink(path, []byte("WEBVTT\n"))
}

func appendTranscript(path string, entry TranscriptEntry) error {
	var entries []TranscriptEntry
	if body, err := readFileNoSymlink(path); err == nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &entries); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	entries = append(entries, entry)
	body, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	root, err := archive.RootFromSidecarPath(path)
	if err != nil {
		return err
	}
	if err := archive.EnsureDirNoSymlinks(root, filepath.Dir(path)); err != nil {
		return err
	}
	return writeFileNoSymlink(path, append(body, '\n'))
}

func openAppendNoSymlink(path string) (*os.File, error) {
	before, beforeErr := os.Lstat(path)
	if beforeErr == nil {
		if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
			return nil, errors.New("archive sidecar path is not a regular file")
		}
	} else if !os.IsNotExist(beforeErr) {
		return nil, beforeErr
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	if err := verifyOpenRegularFile(path, file, before, beforeErr); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func readFileNoSymlink(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("archive sidecar path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := verifyOpenRegularFile(path, file, info, nil); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func writeFileNoSymlink(path string, body []byte) error {
	root, err := archive.RootFromSidecarPath(path)
	if err != nil {
		return err
	}
	if err := archive.EnsureDirNoSymlinks(root, filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("archive sidecar path is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive sidecar path is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func verifyOpenRegularFile(path string, file *os.File, before os.FileInfo, beforeErr error) error {
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
		return errors.New("archive sidecar path is not a regular file")
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return errors.New("archive sidecar path is not a regular file")
	}
	if beforeErr == nil && before != nil && !os.SameFile(before, after) {
		return errors.New("archive sidecar path changed while opening")
	}
	if !os.SameFile(opened, after) {
		return errors.New("archive sidecar path changed while opening")
	}
	return nil
}

func safePayload(payload map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range payload {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if secretPayloadKey(key) {
			out[key] = "<redacted>"
			continue
		}
		out[key] = safePayloadValue(value)
	}
	return out
}

func safePayloadValue(value any) any {
	switch typed := value.(type) {
	case string:
		if secretPayloadValue(typed) {
			return "<redacted>"
		}
		if len(typed) > 500 {
			return typed[:500] + "..."
		}
		return typed
	case bool:
		return typed
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return typed
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, safePayloadValue(item))
		}
		return out
	case map[string]any:
		return safePayload(typed)
	default:
		return nil
	}
}

func secretPayloadKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, token := range []string{"token", "secret", "password", "passwd", "private_key", "credential", "authorization", "stream_key", "refresh_token", "access_token", "client_secret", "api_key", "apikey", "webhook_url"} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

var secretPayloadPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)https://discord\.com/api/webhooks/[0-9A-Za-z_-]+/[0-9A-Za-z_-]+`),
	regexp.MustCompile(`(?i)https://hooks\.slack\.com/services/[0-9A-Za-z/_-]+`),
	regexp.MustCompile(`(?i)\bast_svc_[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`(?i)\bast_ingest_v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	regexp.MustCompile(`(?i)\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`(?i)\b1//[0-9A-Za-z_-]{20,}\b`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{12,}\b`),
	regexp.MustCompile(`(?i)(?:rtmp|rtmps|rtsp|srt|https?)://[^ \t\r\n<>"']+:[^ \t\r\n<>"']+@[^ \t\r\n<>"']+`),
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
}

func secretPayloadValue(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false
	}
	for _, token := range []string{"token=", "access_token=", "refresh_token=", "client_secret=", "private_key", "password=", "passwd=", "stream_key="} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	for _, pattern := range secretPayloadPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func formatVTTTime(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", int(t.Sub(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)).Hours()), t.Minute(), t.Second(), t.Nanosecond()/1_000_000)
}

func sanitizeCaptionText(text string) string {
	text = strings.ReplaceAll(text, "\r", "")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n")
}
