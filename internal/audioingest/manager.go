package audioingest

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	pionopus "github.com/pion/opus"
)

const (
	audioRTPFrameInterval    = 20 * time.Millisecond
	audioRTPClockStep        = uint32(960)
	audioRTPPayloadType      = byte(96)
	audioRTPSSRC             = uint32(0x41535452)
	audioRTPQueueSize        = 2048
	audioSamplesPerFrame     = 960
	audioChannels            = 2
	audioBytesPerSample      = 2
	audioPCMFrameSamples     = audioSamplesPerFrame * audioChannels
	audioPCMFrameBytes       = audioPCMFrameSamples * audioBytesPerSample
	audioMaxOpusFrameSamples = 5760 * audioChannels
	audioSpeakerQueueSize    = 25
	audioMaxTrackedSpeakers  = 24
	audioSpeakerIdleTimeout  = 30 * time.Second
)

// Discord may omit packets while a channel is silent. Keep FFmpeg's local RTP
// clock alive with one 20 ms stereo PCM frame even when no speaker contributes.
var pcmSilenceFrame = make([]byte, audioPCMFrameBytes)

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
	bridges     map[string]*bridgeRecord
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

type bridgeRecord struct {
	conn           *net.UDPConn
	target         *net.UDPAddr
	packets        chan queuedOpusPacket
	decoderFactory func() (opusPCMDecoder, error)
	stop           chan struct{}
	done           chan struct{}
	stopOnce       sync.Once

	trackedSpeakers        atomic.Int64
	mixedFramesTotal       atomic.Int64
	decodeErrorsTotal      atomic.Int64
	queueDropsTotal        atomic.Int64
	speakerLimitDropsTotal atomic.Int64
	clippingPreventedTotal atomic.Int64
}

type queuedOpusPacket struct {
	ssrc      uint32
	sequence  uint16
	timestamp uint32
	opus      []byte
}

type opusPCMDecoder interface {
	DecodeToInt16(in []byte, out []int16) (int, error)
}

type speakerDecoder struct {
	decoder          opusPCMDecoder
	queue            []queuedOpusPacket
	pcm              []int16
	lastSeen         time.Time
	expectedSequence uint16
	hasSequence      bool
}

type Stats struct {
	StreamID               string    `json:"stream_id"`
	BridgeActive           bool      `json:"bridge_active"`
	StartedAt              time.Time `json:"started_at"`
	LastPacketAt           time.Time `json:"last_packet_at,omitempty"`
	PacketsTotal           int64     `json:"packets_total"`
	RTPForwarded           int64     `json:"rtp_forwarded"`
	LastPacketAgeSec       float64   `json:"last_packet_age_sec"`
	TrackedSpeakers        int       `json:"tracked_speakers"`
	MixedFramesTotal       int64     `json:"mixed_frames_total"`
	DecodeErrorsTotal      int64     `json:"decode_errors_total"`
	QueueDropsTotal        int64     `json:"queue_drops_total"`
	SpeakerLimitDropsTotal int64     `json:"speaker_limit_drops_total"`
	ClippingPreventedTotal int64     `json:"clipping_prevented_total"`
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bridges == nil {
		m.bridges = map[string]*bridgeRecord{}
	}
	if m.bridges[streamID] != nil {
		return Bridge{}, errors.New("audio bridge is already running")
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Bridge{}, err
	}
	conn, err := openRTPSender()
	if err != nil {
		return Bridge{}, err
	}
	port, err := freeUDPPort()
	if err != nil {
		_ = conn.Close()
		return Bridge{}, err
	}
	bridge := Bridge{StreamID: streamID, Port: port, SDPPath: layout.TmpDiscordOpusSDP(), InputURL: "internal_discord_audio:" + filepath.ToSlash(layout.TmpDiscordOpusSDP())}
	if err := writeFileNoSymlink(bridge.SDPPath, []byte(sdpForPort(port))); err != nil {
		_ = conn.Close()
		return Bridge{}, err
	}
	target, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		_ = conn.Close()
		return Bridge{}, err
	}
	record := &bridgeRecord{
		conn:           conn,
		target:         target,
		packets:        make(chan queuedOpusPacket, audioRTPQueueSize),
		decoderFactory: newOpusPCMDecoder,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	if m.stats == nil {
		m.stats = map[string]Stats{}
	}
	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}
	m.bridges[streamID] = record
	stats := m.stats[streamID]
	stats.StreamID = streamID
	stats.BridgeActive = true
	stats.StartedAt = time.Now().UTC()
	m.stats[streamID] = stats
	go record.run()
	return bridge, nil
}

func (m *Manager) StopBridge(streamID string) {
	m.mu.Lock()
	record := m.bridges[streamID]
	delete(m.bridges, streamID)
	if stats, ok := m.stats[streamID]; ok {
		stats.BridgeActive = false
		m.stats[streamID] = stats
	}
	m.mu.Unlock()
	if record != nil {
		record.close()
		m.mu.Lock()
		stats := m.stats[streamID]
		stats.TrackedSpeakers = 0
		stats.MixedFramesTotal += record.mixedFramesTotal.Load()
		stats.DecodeErrorsTotal += record.decodeErrorsTotal.Load()
		stats.QueueDropsTotal += record.queueDropsTotal.Load()
		stats.SpeakerLimitDropsTotal += record.speakerLimitDropsTotal.Load()
		stats.ClippingPreventedTotal += record.clippingPreventedTotal.Load()
		m.stats[streamID] = stats
		m.mu.Unlock()
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
	bridge, active := m.bridges[streamID]
	stats.BridgeActive = active
	if bridge != nil {
		stats.TrackedSpeakers = int(bridge.trackedSpeakers.Load())
		stats.MixedFramesTotal += bridge.mixedFramesTotal.Load()
		stats.DecodeErrorsTotal += bridge.decodeErrorsTotal.Load()
		stats.QueueDropsTotal += bridge.queueDropsTotal.Load()
		stats.SpeakerLimitDropsTotal += bridge.speakerLimitDropsTotal.Load()
		stats.ClippingPreventedTotal += bridge.clippingPreventedTotal.Load()
	}
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
			if bridge.enqueue(queuedOpusPacket{
				ssrc:      decoded.packet.SSRC,
				sequence:  decoded.packet.Sequence,
				timestamp: decoded.packet.Timestamp,
				opus:      decoded.opus,
			}) {
				rtpForwarded++
			}
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
		"s=AutoStream Discord Mixed PCM\n"+
		"c=IN IP4 127.0.0.1\n"+
		"t=0 0\n"+
		"m=audio %d RTP/AVP 96\n"+
		"a=rtpmap:96 L16/48000/2\n"+
		"a=recvonly\n", port)
}

func openRTPSender() (*net.UDPConn, error) {
	// Bind the sender first so freeUDPPort cannot return the same ephemeral port
	// for FFmpeg's receiver. A connected UDP socket opened after freeUDPPort can
	// otherwise reuse that just-released port and make FFmpeg's bind fail.
	return net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
}

func (r *bridgeRecord) run() {
	defer close(r.done)
	defer r.conn.Close()
	ticker := time.NewTicker(audioRTPFrameInterval)
	defer ticker.Stop()
	speakers := make(map[uint32]*speakerDecoder)
	var sequence uint16
	var timestamp uint32
	for {
		select {
		case <-r.stop:
			return
		case packet := <-r.packets:
			r.queueSpeakerPacket(speakers, packet, time.Now())
		case <-ticker.C:
			r.drainPackets(speakers)
			payload := r.mixFrame(speakers, time.Now())
			_, _ = r.conn.WriteToUDP(buildRTPPacket(sequence, timestamp, payload), r.target)
			sequence++
			timestamp += audioRTPClockStep
		}
	}
}

func (r *bridgeRecord) enqueue(packet queuedOpusPacket) bool {
	packet.opus = append([]byte(nil), packet.opus...)
	select {
	case <-r.stop:
		return false
	default:
	}
	select {
	case r.packets <- packet:
		return true
	default:
	}
	// Keep latency bounded even if an HTTP burst exceeds the mixer intake.
	select {
	case <-r.packets:
		r.queueDropsTotal.Add(1)
	default:
	}
	select {
	case r.packets <- packet:
		return true
	default:
		r.queueDropsTotal.Add(1)
		return false
	}
}

func newOpusPCMDecoder() (opusPCMDecoder, error) {
	decoder, err := pionopus.NewDecoderWithOutput(48000, audioChannels)
	if err != nil {
		return nil, err
	}
	return &decoder, nil
}

func (r *bridgeRecord) drainPackets(speakers map[uint32]*speakerDecoder) {
	for {
		select {
		case packet := <-r.packets:
			r.queueSpeakerPacket(speakers, packet, time.Now())
		default:
			return
		}
	}
}

func (r *bridgeRecord) queueSpeakerPacket(speakers map[uint32]*speakerDecoder, packet queuedOpusPacket, now time.Time) {
	r.evictIdleSpeakers(speakers, now)
	speaker := speakers[packet.ssrc]
	if speaker == nil {
		if len(speakers) >= audioMaxTrackedSpeakers {
			r.speakerLimitDropsTotal.Add(1)
			return
		}
		decoder, err := r.decoderFactory()
		if err != nil {
			r.decodeErrorsTotal.Add(1)
			return
		}
		speaker = &speakerDecoder{decoder: decoder}
		speakers[packet.ssrc] = speaker
		r.trackedSpeakers.Store(int64(len(speakers)))
	}
	speaker.lastSeen = now
	if len(speaker.queue) >= audioSpeakerQueueSize {
		dropped := len(speaker.queue) - audioSpeakerQueueSize + 1
		copy(speaker.queue, speaker.queue[dropped:])
		speaker.queue = speaker.queue[:len(speaker.queue)-dropped]
		speaker.pcm = speaker.pcm[:0]
		speaker.hasSequence = false
		if decoder, err := r.decoderFactory(); err == nil {
			speaker.decoder = decoder
		} else {
			r.decodeErrorsTotal.Add(1)
		}
		r.queueDropsTotal.Add(int64(dropped))
	}
	speaker.queue = append(speaker.queue, packet)
}

func (r *bridgeRecord) evictIdleSpeakers(speakers map[uint32]*speakerDecoder, now time.Time) {
	for ssrc, speaker := range speakers {
		if len(speaker.queue) == 0 && len(speaker.pcm) == 0 && now.Sub(speaker.lastSeen) >= audioSpeakerIdleTimeout {
			delete(speakers, ssrc)
		}
	}
	r.trackedSpeakers.Store(int64(len(speakers)))
}

func (r *bridgeRecord) mixFrame(speakers map[uint32]*speakerDecoder, now time.Time) []byte {
	r.evictIdleSpeakers(speakers, now)
	ssrcs := make([]uint32, 0, len(speakers))
	for ssrc := range speakers {
		ssrcs = append(ssrcs, ssrc)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return ssrcs[i] < ssrcs[j] })

	mixed := make([]int64, audioPCMFrameSamples)
	contributors := 0
	for _, ssrc := range ssrcs {
		frame, ok := r.nextSpeakerFrame(speakers[ssrc])
		if !ok {
			continue
		}
		contributors++
		for i, sample := range frame {
			mixed[i] += int64(sample)
		}
	}
	if contributors == 0 {
		return pcmSilenceFrame
	}
	r.mixedFramesTotal.Add(1)

	scale := 1 / math.Sqrt(float64(contributors))
	peak := float64(0)
	for _, sample := range mixed {
		magnitude := math.Abs(float64(sample) * scale)
		if magnitude > peak {
			peak = magnitude
		}
	}
	if peak > math.MaxInt16 {
		scale *= float64(math.MaxInt16) / peak
		r.clippingPreventedTotal.Add(1)
	}
	payload := make([]byte, audioPCMFrameBytes)
	for i, sample := range mixed {
		value := int64(math.Round(float64(sample) * scale))
		if value > math.MaxInt16 {
			value = math.MaxInt16
		} else if value < math.MinInt16 {
			value = math.MinInt16
		}
		binary.BigEndian.PutUint16(payload[i*audioBytesPerSample:], uint16(int16(value)))
	}
	return payload
}

func (r *bridgeRecord) nextSpeakerFrame(speaker *speakerDecoder) ([]int16, bool) {
	for len(speaker.pcm) < audioPCMFrameSamples && len(speaker.queue) > 0 {
		packet := speaker.queue[0]
		speaker.queue = speaker.queue[1:]
		if speaker.hasSequence && packet.sequence != speaker.expectedSequence {
			decoder, err := r.decoderFactory()
			if err != nil {
				r.decodeErrorsTotal.Add(1)
				continue
			}
			speaker.decoder = decoder
			speaker.pcm = speaker.pcm[:0]
		}
		speaker.expectedSequence = packet.sequence + 1
		speaker.hasSequence = true

		decoded := make([]int16, audioMaxOpusFrameSamples)
		samplesPerChannel, err := speaker.decoder.DecodeToInt16(packet.opus, decoded)
		if err != nil || samplesPerChannel <= 0 || samplesPerChannel*audioChannels > len(decoded) {
			r.decodeErrorsTotal.Add(1)
			decoder, resetErr := r.decoderFactory()
			if resetErr == nil {
				speaker.decoder = decoder
			} else {
				r.decodeErrorsTotal.Add(1)
			}
			speaker.pcm = speaker.pcm[:0]
			speaker.hasSequence = false
			continue
		}
		speaker.pcm = append(speaker.pcm, decoded[:samplesPerChannel*audioChannels]...)
	}
	if len(speaker.pcm) == 0 {
		return nil, false
	}
	frame := make([]int16, audioPCMFrameSamples)
	count := min(len(speaker.pcm), len(frame))
	copy(frame, speaker.pcm[:count])
	speaker.pcm = speaker.pcm[count:]
	return frame, true
}

func (r *bridgeRecord) close() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func buildRTPPacket(sequence uint16, timestamp uint32, payload []byte) []byte {
	rtp := make([]byte, 12+len(payload))
	rtp[0] = 0x80
	rtp[1] = audioRTPPayloadType
	binary.BigEndian.PutUint16(rtp[2:4], sequence)
	binary.BigEndian.PutUint32(rtp[4:8], timestamp)
	binary.BigEndian.PutUint32(rtp[8:12], audioRTPSSRC)
	copy(rtp[12:], payload)
	return rtp
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
