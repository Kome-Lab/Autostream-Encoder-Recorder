package audioingest

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

type Packet struct {
	SSRC       uint32    `json:"ssrc"`
	UserID     string    `json:"user_id,omitempty"`
	Sequence   uint16    `json:"sequence"`
	Timestamp  uint32    `json:"timestamp"`
	ReceivedAt time.Time `json:"received_at"`
	OpusBase64 string    `json:"opus_base64"`
}

type IngestRequest struct {
	StreamID string   `json:"stream_id"`
	Source   string   `json:"source"`
	Packets  []Packet `json:"packets"`
}

type Result struct {
	Accepted       bool   `json:"accepted"`
	StreamID       string `json:"stream_id"`
	AcceptedCount  int    `json:"accepted_count"`
	DuplicateCount int    `json:"duplicate_count,omitempty"`
	RTPForwarded   int    `json:"rtp_forwarded"`
	AudioArtifact  string `json:"audio_artifact,omitempty"`
}

type Manager struct {
	ArchiveRoot string
	MaxPackets  int
	MaxOpusSize int
	mu          sync.Mutex
	bridges     map[string]Bridge
	stats       map[string]Stats
	seen        map[string]time.Time
	maxSeen     int
}

type Bridge struct {
	StreamID string `json:"stream_id"`
	Port     int    `json:"port"`
	SDPPath  string `json:"-"`
	InputURL string `json:"-"`
}

type Stats struct {
	StreamID         string    `json:"stream_id"`
	BridgeActive     bool      `json:"bridge_active"`
	StartedAt        time.Time `json:"started_at"`
	LastPacketAt     time.Time `json:"last_packet_at,omitempty"`
	PacketsTotal     int64     `json:"packets_total"`
	RTPForwarded     int64     `json:"rtp_forwarded"`
	LastPacketAgeSec float64   `json:"last_packet_age_sec"`
}

func NewManager(archiveRoot string) *Manager {
	if archiveRoot == "" {
		archiveRoot = "/var/lib/autostream/archives"
	}
	return &Manager{ArchiveRoot: archiveRoot, MaxPackets: 100, MaxOpusSize: 4096, seen: map[string]time.Time{}, maxSeen: 10000}
}

func (m *Manager) StartBridge(streamID string) (Bridge, error) {
	if strings.TrimSpace(streamID) == "" {
		return Bridge{}, errors.New("stream_id is required")
	}
	layout, err := archive.NewLayout(m.ArchiveRoot, streamID)
	if err != nil {
		return Bridge{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Bridge{}, err
	}
	port, err := freeUDPPort()
	if err != nil {
		return Bridge{}, err
	}
	bridge := Bridge{StreamID: streamID, Port: port, SDPPath: layout.TmpDiscordOpusSDP(), InputURL: "internal_discord_audio:" + filepath.ToSlash(layout.TmpDiscordOpusSDP())}
	if err := writeFileNoSymlink(bridge.SDPPath, []byte(sdpForPort(port))); err != nil {
		return Bridge{}, err
	}
	m.mu.Lock()
	if m.bridges == nil {
		m.bridges = map[string]Bridge{}
	}
	if m.stats == nil {
		m.stats = map[string]Stats{}
	}
	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}
	m.bridges[streamID] = bridge
	stats := m.stats[streamID]
	stats.StreamID = streamID
	stats.BridgeActive = true
	stats.StartedAt = time.Now().UTC()
	m.stats[streamID] = stats
	m.mu.Unlock()
	return bridge, nil
}

func (m *Manager) StopBridge(streamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.bridges, streamID)
	if stats, ok := m.stats[streamID]; ok {
		stats.BridgeActive = false
		m.stats[streamID] = stats
	}
}

func (m *Manager) Status(streamID string, now time.Time) Stats {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := m.stats[streamID]
	stats.StreamID = streamID
	_, stats.BridgeActive = m.bridges[streamID]
	if stats.StartedAt.IsZero() {
		stats.StartedAt = now
	}
	if stats.LastPacketAt.IsZero() {
		stats.LastPacketAgeSec = now.Sub(stats.StartedAt).Seconds()
	} else {
		stats.LastPacketAgeSec = now.Sub(stats.LastPacketAt).Seconds()
	}
	if stats.LastPacketAgeSec < 0 {
		stats.LastPacketAgeSec = 0
	}
	return stats
}

func (m *Manager) Add(req IngestRequest) (Result, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return Result{}, errors.New("stream_id is required")
	}
	if strings.TrimSpace(req.Source) == "" {
		return Result{}, errors.New("source is required")
	}
	maxPackets := m.MaxPackets
	if maxPackets <= 0 {
		maxPackets = 100
	}
	if len(req.Packets) == 0 || len(req.Packets) > maxPackets {
		return Result{}, errors.New("packets count is invalid")
	}
	maxOpusSize := m.MaxOpusSize
	if maxOpusSize <= 0 {
		maxOpusSize = 4096
	}
	layout, err := archive.NewLayout(m.ArchiveRoot, req.StreamID)
	if err != nil {
		return Result{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Result{}, err
	}

	type decodedPacket struct {
		packet Packet
		opus   []byte
	}
	decodedPackets := make([]decodedPacket, 0, len(req.Packets))
	records := make([]map[string]any, 0, len(req.Packets))
	for _, packet := range req.Packets {
		if strings.TrimSpace(packet.OpusBase64) == "" {
			return Result{}, errors.New("opus_base64 is required")
		}
		decoded, err := base64.StdEncoding.DecodeString(packet.OpusBase64)
		if err != nil {
			return Result{}, errors.New("opus_base64 is invalid")
		}
		if len(decoded) == 0 || len(decoded) > maxOpusSize {
			return Result{}, errors.New("opus packet size is invalid")
		}
		receivedAt := packet.ReceivedAt
		if receivedAt.IsZero() {
			receivedAt = time.Now().UTC()
		}
		packet.ReceivedAt = receivedAt
		records = append(records, map[string]any{
			"timestamp":   receivedAt.Format(time.RFC3339Nano),
			"event":       "discord.opus_packet.received",
			"stream_id":   req.StreamID,
			"source":      req.Source,
			"ssrc":        packet.SSRC,
			"user_id":     packet.UserID,
			"sequence":    packet.Sequence,
			"rtp_ts":      packet.Timestamp,
			"opus_base64": packet.OpusBase64,
		})
		decodedPackets = append(decodedPackets, decodedPacket{packet: packet, opus: decoded})
	}

	m.mu.Lock()
	bridge, hasBridge := m.bridges[req.StreamID]
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}
	acceptedRecords := make([]map[string]any, 0, len(records))
	acceptedPackets := make([]decodedPacket, 0, len(decodedPackets))
	duplicateCount := 0
	for i, decoded := range decodedPackets {
		key := packetDedupeKey(req.StreamID, decoded.packet)
		if _, ok := m.seen[key]; ok {
			duplicateCount++
			continue
		}
		m.seen[key] = decoded.packet.ReceivedAt
		acceptedPackets = append(acceptedPackets, decoded)
		acceptedRecords = append(acceptedRecords, records[i])
	}
	m.trimSeenLocked()
	if len(acceptedRecords) == 0 {
		return Result{Accepted: true, StreamID: req.StreamID, AcceptedCount: 0, DuplicateCount: duplicateCount, AudioArtifact: "discord-opus.jsonl"}, nil
	}
	rtpForwarded := 0
	for _, record := range acceptedRecords {
		if err := appendJSONL(layout.TmpDiscordOpus(), record); err != nil {
			return Result{}, err
		}
	}
	if hasBridge {
		for _, decoded := range acceptedPackets {
			if err := forwardRTP(bridge.Port, decoded.packet, decoded.opus); err != nil {
				return Result{}, err
			}
			rtpForwarded++
		}
	}
	if m.stats == nil {
		m.stats = map[string]Stats{}
	}
	stats := m.stats[req.StreamID]
	stats.StreamID = req.StreamID
	stats.BridgeActive = hasBridge
	if stats.StartedAt.IsZero() {
		stats.StartedAt = time.Now().UTC()
	}
	stats.PacketsTotal += int64(len(acceptedRecords))
	stats.RTPForwarded += int64(rtpForwarded)
	stats.LastPacketAt = time.Now().UTC()
	stats.LastPacketAgeSec = 0
	m.stats[req.StreamID] = stats
	if err := appendJSONL(layout.TmpLogs(), map[string]any{
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
		"event":           "discord.audio_ingest.accepted",
		"stream_id":       req.StreamID,
		"source":          req.Source,
		"accepted_count":  len(acceptedRecords),
		"duplicate_count": duplicateCount,
		"rtp_forwarded":   rtpForwarded,
	}); err != nil {
		return Result{}, err
	}
	return Result{Accepted: true, StreamID: req.StreamID, AcceptedCount: len(acceptedRecords), DuplicateCount: duplicateCount, RTPForwarded: rtpForwarded, AudioArtifact: "discord-opus.jsonl"}, nil
}

func packetDedupeKey(streamID string, packet Packet) string {
	return strings.Join([]string{
		streamID,
		fmt.Sprintf("%d", packet.SSRC),
		fmt.Sprintf("%d", packet.Sequence),
		fmt.Sprintf("%d", packet.Timestamp),
	}, "\x00")
}

func (m *Manager) trimSeenLocked() {
	limit := m.maxSeen
	if limit <= 0 {
		limit = 10000
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

func freeUDPPort() (int, error) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port, nil
}

func sdpForPort(port int) string {
	return fmt.Sprintf("v=0\n"+
		"o=- 0 0 IN IP4 127.0.0.1\n"+
		"s=AutoStream Discord Opus\n"+
		"c=IN IP4 127.0.0.1\n"+
		"t=0 0\n"+
		"m=audio %d RTP/AVP 111\n"+
		"a=rtpmap:111 opus/48000/2\n"+
		"a=fmtp:111 minptime=10;useinbandfec=1\n"+
		"a=recvonly\n", port)
}

func forwardRTP(port int, packet Packet, opus []byte) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	rtp := make([]byte, 12+len(opus))
	rtp[0] = 0x80
	rtp[1] = 111
	binary.BigEndian.PutUint16(rtp[2:4], packet.Sequence)
	binary.BigEndian.PutUint32(rtp[4:8], packet.Timestamp)
	binary.BigEndian.PutUint32(rtp[8:12], packet.SSRC)
	copy(rtp[12:], opus)
	_, err = conn.Write(rtp)
	return err
}

func appendJSONL(path string, value any) error {
	body, err := json.Marshal(value)
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
	file, err := openAppendNoSymlink(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(body, '\n'))
	return err
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
