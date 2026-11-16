package audioingest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycleparticipant"
)

type faultV3File struct {
	*os.File
	mode string
}

func (f *faultV3File) Write(b []byte) (int, error) {
	if f.mode == "partial" {
		n, e := f.File.Write(b[:len(b)/2])
		if e != nil {
			return n, e
		}
		return n, errors.New("synthetic partial write")
	}
	return f.File.Write(b)
}
func (f *faultV3File) Sync() error {
	if f.mode == "sync" {
		return errors.New("synthetic sync failure")
	}
	return f.File.Sync()
}
func (f *faultV3File) Close() error {
	e := f.File.Close()
	if f.mode == "close" {
		return errors.New("synthetic close acknowledgement loss")
	}
	return e
}
func rawFixture(t *testing.T) (*Manager, *SyntheticQueueFixture, *lifecycleparticipant.Participant, lifecycle.Record, IngestRequest) {
	t.Helper()
	var s lifecycleparticipant.OwnerSnapshot
	var r lifecycle.Record
	for name, target := range map[string]any{"owner.json": &s, "request.json": &r} {
		b, e := os.ReadFile(filepath.Join("..", "lifecycleparticipant", "testdata", name))
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(b, target); e != nil {
			t.Fatal(e)
		}
	}
	p, e := lifecycleparticipant.NewSyntheticParticipant(lifecycleparticipant.SyntheticConfig{Environment: "ISOLATED_TEST_ONLY", JournalDir: t.TempDir(), Current: s})
	if e != nil {
		t.Fatal(e)
	}
	m, q, e := NewSyntheticQueueFixture(filepath.Join(t.TempDir(), "raw"), "stream-1", 1)
	if e != nil {
		t.Fatal(e)
	}
	req := IngestRequest{StreamID: "stream-1", Source: "bot-1", Packets: []Packet{{SSRC: 1, Sequence: 1, Timestamp: 960, ReceivedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), OpusBase64: "AQID"}}}
	return m, q, p, r, req
}
func rawMetadata(t *testing.T, r lifecycle.Record) []byte {
	t.Helper()
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func rawObservation(t *testing.T, r lifecycle.Record, sub lifecycle.SubBoundary) lifecycle.BoundaryObservation {
	t.Helper()
	if r.Inspection == nil {
		t.Fatalf("no typed record: %+v", r)
	}
	for _, o := range r.Inspection.Observations.Observations {
		if o.Address.SubBoundary == sub {
			return o
		}
	}
	t.Fatalf("missing boundary %s", sub)
	return lifecycle.BoundaryObservation{}
}
func TestIUWS3RawFaultBoundaries(t *testing.T) {
	for _, mode := range []string{"partial", "sync", "close", "queue_failure", "partial_queue", "seen_before_failure"} {
		t.Run(mode, func(t *testing.T) {
			m, q, p, request, body := rawFixture(t)
			m.v3 = &v3State{attempts: map[string]lifecycle.Correlation{}, open: func(path string) (v3RawFile, error) {
				f, e := openAppendNoSymlink(path)
				if e != nil {
					return nil, e
				}
				return &faultV3File{File: f, mode: mode}, nil
			}}
			if mode == "queue_failure" {
				q.CloseAdmission()
			}
			if mode == "partial_queue" {
				next := body.Packets[0]
				next.Sequence = 2
				next.Timestamp = 1920
				body.Packets = append(body.Packets, next)
			}
			if mode == "seen_before_failure" {
				m.seen[packetDedupeKey(body.StreamID, body.Packets[0])] = body.Packets[0].ReceivedAt
			}
			rec, check := m.AddV3(context.Background(), p, rawMetadata(t, request), body)
			if rec.Inspection == nil || check.Kind == lifecycle.StructurallyCoherent {
				t.Fatalf("failure/unknown was success: %+v", check)
			}
			raw := rawObservation(t, rec, lifecycle.RawWrite)
			durability := rawObservation(t, rec, lifecycle.Durability)
			queue := rawObservation(t, rec, lifecycle.QueueAdmission)
			if mode == "seen_before_failure" {
				if raw.Outcome.Value == nil || *raw.Outcome.Value != lifecycle.NoEffectProven || q.Pending() != 0 {
					t.Fatal("seen marker converted to duplicate success")
				}
				if _, e := os.Stat(m.ArchiveRoot); !os.IsNotExist(e) {
					t.Fatal("seen marker created raw directory")
				}
				return
			}
			if mode == "partial" {
				if raw.Outcome.Value == nil || *raw.Outcome.Value != lifecycle.FailureAtBoundary || raw.Details.RawExtent.Length == nil || *raw.Details.RawExtent.Length == 0 || *raw.Details.RawExtent.Length >= *raw.Details.RawExtent.RequestedLength {
					t.Fatalf("partial extent lost: %+v", raw)
				}
			}
			if mode == "partial" || mode == "sync" || mode == "close" {
				if durability.Knowledge != lifecycle.KnowledgeUnknown || q.Pending() != 0 {
					t.Fatal("failed raw durability reached queue")
				}
			} else {
				if queue.Outcome.Value == nil || *queue.Outcome.Value != lifecycle.FailureAtBoundary {
					t.Fatalf("queue failure hidden: %+v", queue)
				}
			}
			layout, e := archive.NewLayout(m.ArchiveRoot, body.StreamID)
			if e != nil {
				t.Fatal(e)
			}
			before, e := os.ReadFile(layout.TmpDiscordOpus())
			if e != nil {
				t.Fatal(e)
			}
			request.Header.Correlation.AttemptID = "attempt-different"
			request.Header.Correlation.ExecutionKey = "execution-different"
			request.Header.RecordKey.RecordID = "different-record"
			duplicate, _ := m.AddV3(context.Background(), p, rawMetadata(t, request), body)
			d := rawObservation(t, duplicate, lifecycle.RawWrite)
			if d.Outcome.Value == nil || *d.Outcome.Value != lifecycle.NoEffectProven {
				t.Fatal("failed earlier attempt became duplicate success")
			}
			after, e := os.ReadFile(layout.TmpDiscordOpus())
			if e != nil || string(before) != string(after) {
				t.Fatal("different attempt blindly reappended after partial/unknown result")
			}
		})
	}
}
