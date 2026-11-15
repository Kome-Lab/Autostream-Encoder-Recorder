package audioingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycleparticipant"
)

type v3RawFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Stat() (os.FileInfo, error)
}
type v3State struct {
	attempts map[string]lifecycle.Correlation
	open     func(string) (v3RawFile, error)
}

// SyntheticQueueFixture is an explicit isolated queue, without a decoder/RTP or
// provider. Ordinary NewManager/StartBridge do not select this fixture.
type SyntheticQueueFixture struct {
	manager *Manager
	stream  string
	bridge  *bridgeRecord
}

func NewSyntheticQueueFixture(root, stream string, capacity int) (*Manager, *SyntheticQueueFixture, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(stream) == "" || capacity <= 0 {
		return nil, nil, errors.New("explicit fixture root/stream/capacity required")
	}
	m := NewManager(root)
	b := &bridgeRecord{packets: make(chan queuedOpusPacket, capacity), stop: make(chan struct{})}
	b.done = make(chan struct{})
	close(b.done)
	m.bridges = map[string]*bridgeRecord{stream: b}
	return m, &SyntheticQueueFixture{manager: m, stream: stream, bridge: b}, nil
}
func (f *SyntheticQueueFixture) ConsumeOne() bool {
	if f == nil {
		return false
	}
	select {
	case <-f.bridge.packets:
		return true
	default:
		return false
	}
}
func (f *SyntheticQueueFixture) Pending() int {
	if f == nil {
		return 0
	}
	return len(f.bridge.packets)
}
func (f *SyntheticQueueFixture) CloseAdmission() {
	if f != nil {
		f.bridge.stopOnce.Do(func() { close(f.bridge.stop) })
	}
}
func localUnknown(a lifecycleparticipant.Action, sub lifecycle.SubBoundary, code lifecycle.ID) lifecycle.BoundaryObservation {
	r := a.RawResource
	if sub == lifecycle.QueueAdmission || sub == lifecycle.QueueTerminal {
		r = a.QueueResource
	}
	return lifecycle.BoundaryObservation{Address: lifecycle.BoundaryAddress{Kind: lifecycle.LocalPersistence, OwnerID: r.OwnerID, Resource: r, SubBoundary: sub}, Knowledge: lifecycle.KnowledgeUnknown, Presence: lifecycle.NotObserved, Outcome: lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: code}}}
}
func localKnown(a lifecycleparticipant.Action, sub lifecycle.SubBoundary, v lifecycle.Outcome) lifecycle.BoundaryObservation {
	o := localUnknown(a, sub, "UNUSED")
	o.Knowledge = lifecycle.Known
	o.Presence = lifecycle.Present
	o.Outcome = lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Observed, Value: &v, Observation: &a.Context}
	if v == lifecycle.NoEffectProven {
		o.DetailRef = "local-no-effect-witness"
	}
	return o
}
func queueFact(a lifecycleparticipant.Action, sub lifecycle.SubBoundary, v lifecycle.Outcome) lifecycle.BoundaryObservation {
	o := localKnown(a, sub, v)
	o.Details = &lifecycle.BoundaryDetails{Queue: &lifecycle.QueueDetail{Work: a.QueueWork, ReceiptRef: lifecycle.ID(string(a.QueueWork.WorkID) + "-local")}}
	return o
}
func rawFact(a lifecycleparticipant.Action, v lifecycle.Outcome, offset, n, requested uint64) lifecycle.BoundaryObservation {
	o := localKnown(a, lifecycle.RawWrite, v)
	o.Details = &lifecycle.BoundaryDetails{RawExtent: &lifecycle.WriteExtent{ExtentRef: a.Work.WorkID, Offset: &offset, Length: &n, RequestedLength: &requested}}
	return o
}
func pendingQueue(a lifecycleparticipant.Action) lifecycle.PhysicalObligation {
	return lifecycle.PhysicalObligation{ObligationID: a.QueueWork.WorkID, Kind: lifecycle.QueueObligation, OwnerID: a.QueueResource.OwnerID, Resource: a.QueueResource, WorkRef: a.QueueWork.WorkID, Knowledge: lifecycle.KnowledgeUnknown, Terminal: lifecycle.Fact[lifecycle.ID]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: "QUEUE_TERMINAL_NOT_OBSERVED"}}}
}
func noEffect(a lifecycleparticipant.Action, code lifecycle.ID) lifecycle.ObservationSet {
	return lifecycle.ObservationSet{Gate: lifecycle.Held, Observations: []lifecycle.BoundaryObservation{rawFact(a, lifecycle.NoEffectProven, 0, 0, 0), localUnknown(a, lifecycle.Durability, code), queueFact(a, lifecycle.QueueAdmission, lifecycle.NoEffectProven), queueFact(a, lifecycle.QueueTerminal, lifecycle.NoEffectProven)}}
}

// AddV3 reaches the real archive and bridge queue, after an explicit private
// participant. v2 Add and its wire/default policy are unchanged.
func (m *Manager) AddV3(ctx context.Context, p *lifecycleparticipant.Participant, metadata []byte, req IngestRequest) (lifecycle.Record, lifecycle.Result) {
	refuse := func(code string) (lifecycle.Record, lifecycle.Result) {
		return lifecycle.Record{}, lifecycle.Result{Kind: lifecycle.RefusedUnchanged, Issues: []lifecycle.Issue{{Code: code, Path: "AudioIngress"}}}
	}
	if m == nil || p == nil {
		return refuse("PARTICIPANT_REQUIRED")
	}
	if req.StreamID == "" || req.Source == "" || m.MaxPackets <= 0 || m.MaxOpusSize <= 0 || len(req.Packets) == 0 || len(req.Packets) > m.MaxPackets {
		return refuse("EXPLICIT_INGRESS_BOUNDS_REQUIRED")
	}
	trusted := p.Snapshot()
	if req.Source != string(trusted.IngressProducer.ServiceID) {
		return refuse("INGRESS_SERVICE_MISMATCH")
	}
	type decoded struct {
		packet Packet
		opus   []byte
	}
	packets := []decoded{}
	body := []byte{}
	for _, packet := range req.Packets {
		opus, e := base64.StdEncoding.DecodeString(packet.OpusBase64)
		if e != nil || len(opus) == 0 || len(opus) > m.MaxOpusSize || packet.ReceivedAt.IsZero() {
			return refuse("EXPLICIT_PACKET_METADATA_REQUIRED")
		}
		b, e := json.Marshal(map[string]any{"timestamp": packet.ReceivedAt.UTC().Format(time.RFC3339Nano), "event": "discord.opus_packet.received", "stream_id": req.StreamID, "source": req.Source, "ssrc": packet.SSRC, "user_id": packet.UserID, "sequence": packet.Sequence, "rtp_ts": packet.Timestamp, "opus_base64": packet.OpusBase64})
		if e != nil {
			return refuse("PACKET_ENCODING_REJECTED")
		}
		body = append(body, b...)
		body = append(body, '\n')
		if uint64(len(body)) > trusted.RawBudgetBytes {
			return refuse("RAW_BYTE_BUDGET_EXCEEDED")
		}
		packets = append(packets, decoded{packet, opus})
	}
	return p.Execute(ctx, req.StreamID, metadata, func(ctx context.Context, a lifecycleparticipant.Action) lifecycle.ObservationSet {
		m.mu.Lock()
		defer m.mu.Unlock()
		if uint64(len(body)) > a.RawBudgetBytes {
			return noEffect(a, "CURRENT_RAW_BYTE_BUDGET_EXCEEDED")
		}
		if ctx.Err() != nil {
			return noEffect(a, "CANCELLED_BEFORE_RAW_WRITE")
		}
		if m.v3 == nil {
			m.v3 = &v3State{attempts: map[string]lifecycle.Correlation{}, open: func(path string) (v3RawFile, error) { return openAppendNoSymlink(path) }}
		}
		// A v2 seen marker is not a raw/durability receipt, especially after failure.
		for _, packet := range packets {
			k := packetDedupeKey(req.StreamID, packet.packet)
			if _, old := m.seen[k]; old {
				return noEffect(a, "PREVIOUS_PACKET_OUTCOME_NOT_PROVEN")
			}
			if _, old := m.v3.attempts[k]; old {
				return noEffect(a, "PREVIOUS_PACKET_ATTEMPT_INSPECT_REQUIRED")
			}
		}
		layout, e := archive.NewLayout(m.ArchiveRoot, req.StreamID)
		if e != nil {
			return noEffect(a, "RAW_PATH_REJECTED")
		}
		if e = archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); e != nil {
			return noEffect(a, "RAW_DIRECTORY_REJECTED")
		}
		for _, packet := range packets {
			m.v3.attempts[packetDedupeKey(req.StreamID, packet.packet)] = lifecycle.Correlation{AttemptID: a.Work.WorkID}
		}
		obs := lifecycle.ObservationSet{Gate: lifecycle.Held}
		raw := localUnknown(a, lifecycle.RawWrite, "RAW_OPEN_UNKNOWN")
		durability := localUnknown(a, lifecycle.Durability, "DURABILITY_NOT_OBSERVED")
		queue := queueFact(a, lifecycle.QueueAdmission, lifecycle.NoEffectProven)
		terminal := queueFact(a, lifecycle.QueueTerminal, lifecycle.NoEffectProven)
		f, e := m.v3.open(layout.TmpDiscordOpus())
		if e != nil {
			raw = rawFact(a, lifecycle.NoEffectProven, 0, 0, uint64(len(body)))
			obs.Observations = []lifecycle.BoundaryObservation{raw, durability, queue, terminal}
			return obs
		}
		info, statErr := f.Stat()
		if statErr != nil || info.Size() < 0 {
			_ = f.Close()
			obs.Observations = []lifecycle.BoundaryObservation{raw, durability, queue, terminal}
			return obs
		}
		offset := uint64(info.Size())
		n, writeErr := f.Write(body)
		if n < 0 || n > len(body) {
			n = 0
			writeErr = io.ErrShortWrite
		}
		rawResult := lifecycle.SuccessAtBoundary
		if writeErr != nil || n != len(body) {
			rawResult = lifecycle.FailureAtBoundary
			if writeErr == nil {
				writeErr = io.ErrShortWrite
			}
		}
		raw = rawFact(a, rawResult, offset, uint64(n), uint64(len(body)))
		var syncErr error
		if writeErr == nil {
			syncErr = f.Sync()
		}
		closeErr := f.Close()
		fdID := lifecycle.ID(string(a.Work.WorkID) + "-fd")
		fd := lifecycle.PhysicalObligation{ObligationID: fdID, Kind: lifecycle.FD, OwnerID: a.RawResource.OwnerID, Resource: a.RawResource, WorkRef: a.Work.WorkID, Knowledge: lifecycle.KnowledgeUnknown, Terminal: lifecycle.Fact[lifecycle.ID]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: "CLOSE_NOT_OBSERVED"}}}
		if closeErr == nil {
			closedFact := lifecycle.ID(string(a.Work.WorkID) + "-file-closed")
			fd.Knowledge = lifecycle.Known
			fd.Terminal = lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &closedFact, Observation: &a.Context}
		}
		obs.PhysicalObligations = append(obs.PhysicalObligations, fd)
		if writeErr == nil && syncErr == nil && closeErr == nil {
			durability = localKnown(a, lifecycle.Durability, lifecycle.SuccessAtBoundary)
			durability.Details = &lifecycle.BoundaryDetails{Durability: &lifecycle.DurabilityDetail{ProfileRef: a.Profile, Level: "SYNC_OBSERVED", WitnessRef: lifecycle.ID(string(a.Work.WorkID) + "-sync-close")}}
		} else {
			id := lifecycle.ID(string(a.Work.WorkID) + "-raw")
			obs.RemainingObligations = append(obs.RemainingObligations, id)
		}
		if durability.Knowledge == lifecycle.Known {
			bridge, exists := m.bridges[req.StreamID]
			forwarded := 0
			if exists {
				for _, packet := range packets {
					if bridge.enqueueV3(queuedOpusPacket{ssrc: packet.packet.SSRC, sequence: packet.packet.Sequence, timestamp: packet.packet.Timestamp, opus: packet.opus}) {
						forwarded++
					}
				}
			}
			if forwarded == len(packets) {
				queue = queueFact(a, lifecycle.QueueAdmission, lifecycle.SuccessAtBoundary)
				terminal = queueFact(a, lifecycle.QueueTerminal, lifecycle.Pending)
			} else {
				queue = queueFact(a, lifecycle.QueueAdmission, lifecycle.FailureAtBoundary)
				if forwarded > 0 {
					terminal = localUnknown(a, lifecycle.QueueTerminal, "PARTIAL_QUEUE_ADMISSION")
				}
			}
			if forwarded > 0 {
				obs.PhysicalObligations = append(obs.PhysicalObligations, pendingQueue(a))
				obs.RemainingObligations = append(obs.RemainingObligations, a.QueueWork.WorkID)
			}
			if m.stats == nil {
				m.stats = map[string]Stats{}
			}
			s := m.stats[req.StreamID]
			s.StreamID = req.StreamID
			s.BridgeActive = exists
			s.PacketsTotal += int64(len(packets))
			s.RTPForwarded += int64(forwarded)
			s.LastPacketAt = time.Now().UTC()
			m.stats[req.StreamID] = s
		}
		obs.Observations = []lifecycle.BoundaryObservation{raw, durability, queue, terminal}
		return obs
	})
}

// Unlike the v2 latency policy, v3 never drops older work to admit a new packet.
func (r *bridgeRecord) enqueueV3(packet queuedOpusPacket) bool {
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
		return false
	}
}
