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

var (
	tone440OpusFrame = []byte{
		0x78, 0x82, 0x01, 0xb7, 0x6c, 0x7e, 0x40, 0xe6, 0x00, 0x00, 0x06, 0x4b, 0xb4, 0xe3, 0xba, 0x66,
		0x11, 0xde, 0x9d, 0x34, 0x46, 0x6c, 0x01, 0x1c, 0xf0, 0x7e, 0x23, 0x59, 0xb6, 0xcc, 0x85, 0xeb,
		0x99, 0xde, 0xda, 0x04, 0x81, 0xa3, 0x76, 0xe0, 0xb6, 0x03, 0x5c, 0x16, 0x7e, 0xe2, 0xa8, 0xa7,
		0x1e, 0xdf, 0xce, 0x5a, 0xd0, 0x4b, 0x86, 0xc6, 0x2d, 0xcf, 0x7f, 0xfa, 0x49, 0xd2, 0x45, 0x49,
		0xd8, 0xc7, 0x09, 0x5d, 0x79,
	}
	tone880OpusFrame = []byte{
		0x78, 0x82, 0xb4, 0x52, 0x81, 0x8b, 0xcf, 0x53, 0xbb, 0x26, 0x9e, 0xb0, 0x81, 0x73, 0xe0, 0xe2,
		0x63, 0x14, 0x18, 0x60, 0x14, 0xea, 0xe0, 0x4e, 0xf9, 0x85, 0x3c, 0xe1, 0xa2, 0x8d, 0x15, 0xf9,
		0x59, 0xf1, 0xbb, 0x52, 0xef, 0x21, 0xc6, 0x32, 0x10, 0x76, 0x08, 0x40, 0xa4, 0x30, 0x24, 0x23,
		0x56, 0xf4, 0x81, 0x77, 0x17, 0x1b, 0x8d, 0x95, 0xb1, 0x67, 0x70, 0xdc, 0x5d,
	}
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
	} else if !strings.Contains(string(body), "L16/48000/2") {
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
	if got := first[12:]; len(got) != audioPCMFrameBytes || !isSilentRTPPayload(got) {
		t.Fatalf("first RTP payload is not one silent PCM frame: bytes=%d", len(got))
	}

	result, err := manager.Add(IngestRequest{
		StreamID: "stream-01",
		Source:   "discord-bot-01",
		Packets: []Packet{{
			SSRC:       9876,
			Sequence:   42,
			Timestamp:  123456,
			ReceivedAt: time.Now().UTC(),
			OpusBase64: base64.StdEncoding.EncodeToString(tone440OpusFrame),
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
		if payload := packet[12:]; len(payload) == audioPCMFrameBytes && !isSilentRTPPayload(payload) {
			break
		}
		previous = packet
	}
}

func TestBridgeMixesConcurrentSpeakersWithoutSerializingTheirTimelines(t *testing.T) {
	solo440 := captureBridgeAudioPayload(t, []Packet{{SSRC: 440, Sequence: 1, Timestamp: 960, OpusBase64: base64.StdEncoding.EncodeToString(tone440OpusFrame)}})
	solo880 := captureBridgeAudioPayload(t, []Packet{{SSRC: 880, Sequence: 1, Timestamp: 960, OpusBase64: base64.StdEncoding.EncodeToString(tone880OpusFrame)}})
	mixed := captureBridgeAudioPayload(t, []Packet{
		{SSRC: 440, Sequence: 1, Timestamp: 960, OpusBase64: base64.StdEncoding.EncodeToString(tone440OpusFrame)},
		{SSRC: 880, Sequence: 1, Timestamp: 960, OpusBase64: base64.StdEncoding.EncodeToString(tone880OpusFrame)},
	})

	if len(mixed) != audioPCMFrameBytes {
		t.Fatalf("mixed RTP payload size = %d, want one 20 ms PCM frame (%d bytes)", len(mixed), audioPCMFrameBytes)
	}
	if bytes.Equal(mixed, solo440) || bytes.Equal(mixed, solo880) {
		t.Fatal("concurrent speakers were serialized instead of mixed into the same audio frame")
	}
}

func TestBridgeSupportsFifteenConcurrentSpeakersWithHeadroom(t *testing.T) {
	packets := make([]Packet, 0, 15)
	for speaker := 0; speaker < 15; speaker++ {
		frame := tone440OpusFrame
		if speaker%2 == 1 {
			frame = tone880OpusFrame
		}
		packets = append(packets, Packet{
			SSRC:       uint32(1000 + speaker),
			Sequence:   1,
			Timestamp:  960,
			OpusBase64: base64.StdEncoding.EncodeToString(frame),
		})
	}
	payload := captureBridgeAudioPayload(t, packets)
	if len(payload) != audioPCMFrameBytes || isSilentRTPPayload(payload) {
		t.Fatalf("15-speaker mix is not one audible 20 ms PCM frame: bytes=%d", len(payload))
	}
	if audioMaxTrackedSpeakers <= len(packets) {
		t.Fatalf("speaker limit %d does not leave headroom above %d speakers", audioMaxTrackedSpeakers, len(packets))
	}
}

func TestBridgeBoundsTrackedSpeakers(t *testing.T) {
	record := &bridgeRecord{decoderFactory: newOpusPCMDecoder}
	speakers := make(map[uint32]*speakerDecoder)
	now := time.Now()
	for i := 0; i < audioMaxTrackedSpeakers+1; i++ {
		record.queueSpeakerPacket(speakers, queuedOpusPacket{
			ssrc:     uint32(i + 1),
			sequence: 1,
			opus:     tone440OpusFrame,
		}, now)
	}
	if got := len(speakers); got != audioMaxTrackedSpeakers {
		t.Fatalf("tracked speakers = %d, want hard limit %d", got, audioMaxTrackedSpeakers)
	}
	if got := record.speakerLimitDropsTotal.Load(); got != 1 {
		t.Fatalf("speaker limit drops = %d, want 1", got)
	}
}

func BenchmarkMixFrameFifteenSpeakers(b *testing.B) {
	for b.Loop() {
		record := &bridgeRecord{decoderFactory: newOpusPCMDecoder}
		speakers := make(map[uint32]*speakerDecoder, 15)
		now := time.Now()
		for speaker := 0; speaker < 15; speaker++ {
			frame := tone440OpusFrame
			if speaker%2 == 1 {
				frame = tone880OpusFrame
			}
			record.queueSpeakerPacket(speakers, queuedOpusPacket{
				ssrc:     uint32(speaker + 1),
				sequence: 1,
				opus:     frame,
			}, now)
		}
		if payload := record.mixFrame(speakers, now); len(payload) != audioPCMFrameBytes {
			b.Fatalf("mixed payload bytes = %d", len(payload))
		}
	}
}

func BenchmarkMixFrameFifteenSpeakersSteadyState(b *testing.B) {
	record := &bridgeRecord{decoderFactory: newOpusPCMDecoder}
	speakers := make(map[uint32]*speakerDecoder, 15)
	sequences := make([]uint16, 15)
	now := time.Now()
	queueFrame := func() {
		for speaker := 0; speaker < 15; speaker++ {
			frame := tone440OpusFrame
			if speaker%2 == 1 {
				frame = tone880OpusFrame
			}
			sequences[speaker]++
			record.queueSpeakerPacket(speakers, queuedOpusPacket{
				ssrc:     uint32(speaker + 1),
				sequence: sequences[speaker],
				opus:     frame,
			}, now)
		}
	}
	queueFrame()
	_ = record.mixFrame(speakers, now)
	b.ResetTimer()
	for b.Loop() {
		queueFrame()
		if payload := record.mixFrame(speakers, now); len(payload) != audioPCMFrameBytes {
			b.Fatalf("mixed payload bytes = %d", len(payload))
		}
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

func captureBridgeAudioPayload(t *testing.T, packets []Packet) []byte {
	t.Helper()
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

	// Synchronize immediately after a bridge tick so every packet in this HTTP
	// batch is available for the next 20 ms mix frame.
	_ = readRTPPacket(t, listener)
	if _, err := manager.Add(IngestRequest{StreamID: "stream-01", Source: "discord-bot-01", Packets: packets}); err != nil {
		t.Fatal(err)
	}
	for {
		payload := readRTPPacket(t, listener)[12:]
		if !isSilentRTPPayload(payload) {
			return append([]byte(nil), payload...)
		}
	}
}

func isSilentRTPPayload(payload []byte) bool {
	for _, value := range payload {
		if value != 0 {
			return false
		}
	}
	return true
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
