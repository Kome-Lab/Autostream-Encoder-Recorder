package audioingest

import "time"

type Stats struct {
	StreamID                string    `json:"stream_id"`
	BridgeActive            bool      `json:"bridge_active"`
	StartedAt               time.Time `json:"started_at"`
	LastPacketAt            time.Time `json:"last_packet_at,omitempty"`
	PacketsTotal            int64     `json:"packets_total"`
	RTPForwarded            int64     `json:"rtp_forwarded"`
	LastPacketAgeSec        float64   `json:"last_packet_age_sec"`
	TrackedSpeakers         int       `json:"tracked_speakers"`
	MixedFramesTotal        int64     `json:"mixed_frames_total"`
	DecodeErrorsTotal       int64     `json:"decode_errors_total"`
	QueueDropsTotal         int64     `json:"queue_drops_total"`
	SpeakerLimitDropsTotal  int64     `json:"speaker_limit_drops_total"`
	ClippingPreventedTotal  int64     `json:"clipping_prevented_total"`
	SequenceGapEventsTotal  int64     `json:"sequence_gap_events_total"`
	MissingPacketsTotal     int64     `json:"missing_packets_total"`
	ConcealedFramesTotal    int64     `json:"concealed_frames_total"`
	LatePacketsDroppedTotal int64     `json:"late_packets_dropped_total"`
	DecoderResetsTotal      int64     `json:"decoder_resets_total"`
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
		stats.SequenceGapEventsTotal += bridge.sequenceGapEventsTotal.Load()
		stats.MissingPacketsTotal += bridge.missingPacketsTotal.Load()
		stats.ConcealedFramesTotal += bridge.concealedFramesTotal.Load()
		stats.LatePacketsDroppedTotal += bridge.latePacketsDroppedTotal.Load()
		stats.DecoderResetsTotal += bridge.decoderResetsTotal.Load()
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
