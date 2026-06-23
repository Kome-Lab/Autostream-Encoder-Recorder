package audioingest

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManagerAcceptsOpusPackets(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	result, err := manager.Add(IngestRequest{
		StreamID: "stream-01",
		Source:   "discord-bot-01",
		Packets: []Packet{{
			SSRC:       1234,
			UserID:     "user-01",
			Sequence:   10,
			Timestamp:  960,
			ReceivedAt: time.Now().UTC(),
			OpusBase64: base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.AcceptedCount != 1 || result.AudioArtifact != "discord-opus.jsonl" {
		t.Fatalf("unexpected result: %#v", result)
	}
	body, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "discord-opus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "discord.opus_packet.received") || !strings.Contains(string(body), "user-01") {
		t.Fatalf("packet was not logged: %s", string(body))
	}
}

func TestManagerStartsBridgeAndForwardsRTP(t *testing.T) {
	manager := NewManager(t.TempDir())
	bridge, err := manager.StartBridge("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Port == 0 || bridge.SDPPath == "" || bridge.InputURL == "" {
		t.Fatalf("unexpected bridge: %#v", bridge)
	}
	if body, err := os.ReadFile(bridge.SDPPath); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(body), "opus/48000/2") {
		t.Fatalf("unexpected sdp: %s", string(body))
	}
	result, err := manager.Add(IngestRequest{
		StreamID: "stream-01",
		Source:   "discord-bot-01",
		Packets: []Packet{{
			SSRC:       1234,
			Sequence:   10,
			Timestamp:  960,
			ReceivedAt: time.Now().UTC(),
			OpusBase64: base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RTPForwarded != 1 {
		t.Fatalf("expected RTP forwarding, got %#v", result)
	}
	stats := manager.Status("stream-01", time.Now().UTC())
	if stats.PacketsTotal != 1 || stats.RTPForwarded != 1 || !stats.BridgeActive {
		t.Fatalf("unexpected stats: %#v", stats)
	}
	manager.StopBridge("stream-01")
	if stats := manager.Status("stream-01", time.Now().UTC()); stats.BridgeActive {
		t.Fatalf("bridge should be stopped: %#v", stats)
	}
}

func TestManagerRejectsInvalidOpusBase64(t *testing.T) {
	manager := NewManager(t.TempDir())
	_, err := manager.Add(IngestRequest{StreamID: "stream-01", Source: "discord-bot-01", Packets: []Packet{{OpusBase64: "not-base64"}}})
	if err == nil {
		t.Fatal("expected invalid base64 to be rejected")
	}
}

func TestManagerDeduplicatesRetriedOpusPackets(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	packet := Packet{
		SSRC:       1234,
		UserID:     "user-01",
		Sequence:   10,
		Timestamp:  960,
		ReceivedAt: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
		OpusBase64: base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
	}
	req := IngestRequest{StreamID: "stream-01", Source: "discord-bot-01", Packets: []Packet{packet}}
	first, err := manager.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.AcceptedCount != 1 || first.DuplicateCount != 0 {
		t.Fatalf("unexpected first ingest result: %#v", first)
	}
	if second.AcceptedCount != 0 || second.DuplicateCount != 1 {
		t.Fatalf("unexpected duplicate ingest result: %#v", second)
	}
	body, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "discord-opus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "discord.opus_packet.received") != 1 {
		t.Fatalf("duplicate opus packet was written more than once: %s", string(body))
	}
	stats := manager.Status("stream-01", packet.ReceivedAt.Add(time.Second))
	if stats.PacketsTotal != 1 || stats.RTPForwarded != 0 {
		t.Fatalf("duplicate packet changed stats: %#v", stats)
	}
}

func TestManagerDeduplicatesRetriedOpusPacketsAcrossChangedSource(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	packet := Packet{
		SSRC:       1234,
		UserID:     "user-01",
		Sequence:   10,
		Timestamp:  960,
		ReceivedAt: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
		OpusBase64: base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
	}
	first, err := manager.Add(IngestRequest{StreamID: "stream-01", Source: "discord-bot-01", Packets: []Packet{packet}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Add(IngestRequest{StreamID: "stream-01", Source: "attacker-controlled-source", Packets: []Packet{packet}})
	if err != nil {
		t.Fatal(err)
	}
	if first.AcceptedCount != 1 || second.AcceptedCount != 0 || second.DuplicateCount != 1 {
		t.Fatalf("expected changed source replay to dedupe: first=%#v second=%#v", first, second)
	}
}

func TestManagerRejectsAudioSidecarSymlink(t *testing.T) {
	root := t.TempDir()
	tmpDir := filepath.Join(root, "tmp", "stream-01")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.jsonl")
	if err := os.WriteFile(target, []byte("outside\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmpDir, "discord-opus.jsonl")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	manager := NewManager(root)
	_, err := manager.Add(IngestRequest{
		StreamID: "stream-01",
		Source:   "discord-bot-01",
		Packets: []Packet{{
			SSRC:       1234,
			Sequence:   10,
			Timestamp:  960,
			ReceivedAt: time.Now().UTC(),
			OpusBase64: base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
		}},
	})
	if err == nil {
		t.Fatal("expected audio sidecar symlink to be rejected")
	}
	body, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "outside\n" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}

func TestManagerRejectsAudioTmpDirectorySymlink(t *testing.T) {
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
	_, err := manager.Add(IngestRequest{
		StreamID: "stream-01",
		Source:   "discord-bot-01",
		Packets: []Packet{{
			SSRC:       1234,
			Sequence:   10,
			Timestamp:  960,
			ReceivedAt: time.Now().UTC(),
			OpusBase64: base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected tmp directory symlink to be rejected, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "discord-opus.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("symlink target should not receive discord-opus.jsonl, stat err=%v", err)
	}
}

func TestManagerRejectsSDPSymlink(t *testing.T) {
	root := t.TempDir()
	tmpDir := filepath.Join(root, "tmp", "stream-01")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.sdp")
	if err := os.WriteFile(target, []byte("outside\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmpDir, "discord-opus.sdp")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	manager := NewManager(root)
	if _, err := manager.StartBridge("stream-01"); err == nil {
		t.Fatal("expected SDP symlink to be rejected")
	}
	body, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "outside\n" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}
