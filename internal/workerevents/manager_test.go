package workerevents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAddCaptionWritesLogsCaptionsAndTranscript(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	event := Event{
		ID:        "event-01",
		StreamID:  "stream-01",
		Type:      "caption.telop",
		Payload:   map[string]any{"text": "こんにちは", "speaker_user_id": "user-01"},
		Timestamp: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
	}
	result, err := manager.Add(event)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.CaptionsArtifact != "captions.vtt" || result.TranscriptArtifact != "transcript.json" || result.LogsArtifact != "logs.jsonl" {
		t.Fatalf("unexpected result: %#v", result)
	}
	captions, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "captions.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captions), "WEBVTT") || !strings.Contains(string(captions), "こんにちは") {
		t.Fatalf("unexpected captions: %s", string(captions))
	}
	var transcript []TranscriptEntry
	body, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &transcript); err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 1 || transcript[0].Text != "こんにちは" || transcript[0].SpeakerUserID != "user-01" {
		t.Fatalf("unexpected transcript: %#v", transcript)
	}
	logs, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), "worker.event.received") {
		t.Fatalf("unexpected logs: %s", string(logs))
	}
}

func TestAddRejectsUnsafeStreamID(t *testing.T) {
	manager := NewManager(t.TempDir())
	_, err := manager.Add(Event{StreamID: "../bad", Type: "overlay.current_time"})
	if err == nil {
		t.Fatal("expected unsafe stream id to be rejected")
	}
}

func TestAddRejectsUnknownEventType(t *testing.T) {
	manager := NewManager(t.TempDir())
	_, err := manager.Add(Event{ID: "event-01", StreamID: "stream-01", Type: "bad.event"})
	if err == nil {
		t.Fatal("expected unknown event type to be rejected")
	}
}

func TestAddRejectsMissingEventID(t *testing.T) {
	manager := NewManager(t.TempDir())
	_, err := manager.Add(Event{StreamID: "stream-01", Type: "overlay.current_time"})
	if err == nil {
		t.Fatal("expected missing event id to be rejected")
	}
}

func TestAddRedactsSecretLikePayloadInLogsRecentAndCaptions(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	event := Event{
		ID:       "event-secret",
		StreamID: "stream-01",
		Type:     "caption.telop",
		Payload: map[string]any{
			"text":             "bearer super-secret-token",
			"webhook_url":      "https://discord.com/api/webhooks/id/raw-secret-token",
			"nested":           map[string]any{"stream_ingest_token": "ast_ingest_v1.abc123.def456"},
			"safe_participant": "speaker-01",
		},
		Timestamp: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
	}
	if _, err := manager.Add(event); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.Join("tmp", "stream-01", "logs.jsonl"),
		filepath.Join("tmp", "stream-01", "captions.vtt"),
		filepath.Join("tmp", "stream-01", "transcript.json"),
	} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.Contains(text, "super-secret-token") || strings.Contains(text, "discord.com/api/webhooks") || strings.Contains(text, "ast_ingest_v1") {
			t.Fatalf("secret-like value leaked in %s: %s", rel, text)
		}
	}
	events, err := manager.Recent("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one recent event, got %#v", events)
	}
	body, _ := json.Marshal(events[0].Payload)
	if strings.Contains(string(body), "super-secret-token") || strings.Contains(string(body), "discord.com/api/webhooks") || strings.Contains(string(body), "ast_ingest_v1") {
		t.Fatalf("secret-like value leaked in recent payload: %s", string(body))
	}
	if !strings.Contains(string(body), "speaker-01") {
		t.Fatalf("safe payload value was not preserved: %s", string(body))
	}
}

func TestRecentReturnsCopy(t *testing.T) {
	manager := NewManager(t.TempDir())
	if _, err := manager.Add(Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time"}); err != nil {
		t.Fatal(err)
	}
	events, err := manager.Recent("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	events[0].Type = "changed"
	again, err := manager.Recent("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Type != "overlay.current_time" {
		t.Fatalf("recent events were not copied: %#v", again)
	}
}

func TestAddDeduplicatesWorkerEventID(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	event := Event{
		ID:        "event-duplicate",
		StreamID:  "stream-01",
		Type:      "caption.final",
		Payload:   map[string]any{"text": "hello"},
		Timestamp: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
	}
	first, err := manager.Add(event)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Add(event)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || !second.Accepted || !second.Duplicate {
		t.Fatalf("unexpected duplicate results: first=%#v second=%#v", first, second)
	}
	logs, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(logs), "worker.event.received") != 1 {
		t.Fatalf("duplicate worker event was logged more than once: %s", string(logs))
	}
	body, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript []TranscriptEntry
	if err := json.Unmarshal(body, &transcript); err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 1 {
		t.Fatalf("duplicate worker event wrote transcript more than once: %#v", transcript)
	}
	events, err := manager.Recent("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("duplicate worker event was remembered more than once: %#v", events)
	}
}

func TestAddRejectsSidecarSymlink(t *testing.T) {
	root := t.TempDir()
	tmpDir := filepath.Join(root, "tmp", "stream-01")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.log")
	if err := os.WriteFile(target, []byte("outside\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmpDir, "logs.jsonl")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	manager := NewManager(root)
	_, err := manager.Add(Event{ID: "event-symlink", StreamID: "stream-01", Type: "overlay.current_time"})
	if err == nil {
		t.Fatal("expected sidecar symlink to be rejected")
	}
	body, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "outside\n" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}

func TestAddRejectsTmpDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "tmp", "stream-01")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	manager := NewManager(root)
	_, err := manager.Add(Event{ID: "event-dir-symlink", StreamID: "stream-01", Type: "overlay.current_time"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected tmp directory symlink to be rejected, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "logs.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("symlink target should not receive logs.jsonl, stat err=%v", err)
	}
}
