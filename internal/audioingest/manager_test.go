package audioingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
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

func TestManagerRejectsDuplicateBridgeWithoutChangingSDP(t *testing.T) {
	manager := NewManager(t.TempDir())
	bridge, err := manager.StartBridge("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.StopBridge("stream-01")

	before, err := os.ReadFile(bridge.SDPPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartBridge("stream-01"); err == nil {
		t.Fatal("duplicate bridge start unexpectedly succeeded")
	}
	after, err := os.ReadFile(bridge.SDPPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("duplicate bridge start changed the active SDP")
	}
}

func TestBridgeEmitsContinuousSilenceAndNormalizesRealOpusTimeline(t *testing.T) {
	manager := NewManager(t.TempDir())
	bridge, err := manager.StartBridge("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.StopBridge("stream-01")

	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: bridge.Port})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}

	first := readRTPPacket(t, listener)
	if !bytes.Equal(first[12:], opusSilenceFrame) {
		t.Fatalf("first RTP payload = %x, want Opus silence %x", first[12:], opusSilenceFrame)
	}

	realOpus := []byte{0x11, 0x22, 0x33, 0x44}
	result, err := manager.Add(IngestRequest{
		StreamID: "stream-01",
		Source:   "discord-bot-01",
		Packets: []Packet{{
			SSRC:       9876,
			Sequence:   42,
			Timestamp:  123456,
			ReceivedAt: time.Now().UTC(),
			OpusBase64: base64.StdEncoding.EncodeToString(realOpus),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RTPForwarded != 1 {
		t.Fatalf("real Opus was not queued for RTP forwarding: %#v", result)
	}

	previous := first
	for {
		packet := readRTPPacket(t, listener)
		if packet[1]&0x7f != audioRTPPayloadType {
			t.Fatalf("RTP payload type = %d, want %d", packet[1]&0x7f, audioRTPPayloadType)
		}
		if got := binary.BigEndian.Uint32(packet[8:12]); got != audioRTPSSRC {
			t.Fatalf("RTP SSRC = %x, want normalized %x", got, audioRTPSSRC)
		}
		if got, want := binary.BigEndian.Uint16(packet[2:4]), binary.BigEndian.Uint16(previous[2:4])+1; got != want {
			t.Fatalf("RTP sequence = %d, want %d", got, want)
		}
		if got, want := binary.BigEndian.Uint32(packet[4:8]), binary.BigEndian.Uint32(previous[4:8])+audioRTPClockStep; got != want {
			t.Fatalf("RTP timestamp = %d, want %d", got, want)
		}
		if bytes.Equal(packet[12:], realOpus) {
			break
		}
		previous = packet
	}
}

func TestBridgeSilenceKeepsFFmpegAudioClockAdvancing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping FFmpeg integration test in short mode")
	}
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg is not installed: %v", err)
	}

	manager := NewManager(t.TempDir())
	bridge, err := manager.StartBridge("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.StopBridge("stream-01")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-protocol_whitelist", "file,udp,rtp", "-i", bridge.SDPPath,
		"-map", "0:a:0", "-t", "0.2", "-f", "null", "-",
	).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("ffmpeg did not advance on bridge silence: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("ffmpeg could not decode bridge silence: %v\n%s", err, output)
	}
}

func readRTPPacket(t *testing.T, listener *net.UDPConn) []byte {
	t.Helper()
	buffer := make([]byte, 4096)
	count, _, err := listener.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if count < 12 {
		t.Fatalf("short RTP packet: %d bytes", count)
	}
	return append([]byte(nil), buffer[:count]...)
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
