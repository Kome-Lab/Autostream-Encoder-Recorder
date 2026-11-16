package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/lifecycleparticipant"
)

func v3HTTPFixture(t *testing.T) (*lifecycleparticipant.Participant, *audioingest.Manager, *audioingest.SyntheticQueueFixture, lifecycle.Record, string) {
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
	root := filepath.Join(t.TempDir(), "raw")
	manager, queue, e := audioingest.NewSyntheticQueueFixture(root, "stream-1", 8)
	if e != nil {
		t.Fatal(e)
	}
	return p, manager, queue, r, root
}
func v3PacketBody(t *testing.T) []byte {
	t.Helper()
	b, e := json.Marshal(audioingest.IngestRequest{StreamID: "stream-1", Source: "bot-1", Packets: []audioingest.Packet{{SSRC: 1, Sequence: 1, Timestamp: 960, ReceivedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), OpusBase64: "AQID"}}})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func v3Header(t *testing.T, r lifecycle.Record) string {
	t.Helper()
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func v3Call(t *testing.T, client *http.Client, url, metadata string, body []byte) (int, []byte) {
	t.Helper()
	req, e := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer synthetic-service-token")
	req.Header.Set(LifecycleMetadataHeader, metadata)
	req.Header.Set("Content-Type", "application/json")
	response, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	b, e := io.ReadAll(response.Body)
	if e != nil {
		t.Fatal(e)
	}
	return response.StatusCode, b
}
func TestIUWS3ActualHTTPIngress(t *testing.T) {
	t.Run("missing_stale_and_nil_before_raw_directory", func(t *testing.T) {
		for _, kind := range []string{"missing", "stale", "nil", "missing_limit"} {
			t.Run(kind, func(t *testing.T) {
				p, m, q, r, root := v3HTTPFixture(t)
				if kind == "nil" {
					p = nil
				}
				bound := int64(65536)
				if kind == "missing_limit" {
					bound = 0
				}
				handler := NewV3AudioIngressHandler(m, p, TokenVerifier{DiscordAudioPlainToken: "synthetic-service-token"}, bound)
				server := httptest.NewServer(handler)
				defer server.Close()
				metadata := v3Header(t, r)
				if kind == "missing" {
					metadata = ""
				}
				if kind == "stale" {
					for i, e := range r.Vector.Vector {
						if e.Key.Axis == lifecycle.ConnectionAxis {
							n := uint64(2)
							e.Value.Uint64 = &n
							r.Vector.Vector[i] = e
						}
					}
					metadata = v3Header(t, r)
				}
				status, _ := v3Call(t, server.Client(), server.URL+"/v3/streams/stream-1/audio/opus", metadata, v3PacketBody(t))
				if status < 400 {
					t.Fatalf("refusal returned success %d", status)
				}
				if _, e := os.Stat(root); !os.IsNotExist(e) {
					t.Fatal("rejected ingress created raw archive directory")
				}
				if q.Pending() != 0 || m.Status("stream-1", time.Now()).PacketsTotal != 0 {
					t.Fatal("rejection touched product queue or stats")
				}
			})
		}
	})
	t.Run("actual_raw_append_queue_and_same_attempt_inspect", func(t *testing.T) {
		p, m, q, r, root := v3HTTPFixture(t)
		server := httptest.NewServer(NewV3AudioIngressHandler(m, p, TokenVerifier{DiscordAudioPlainToken: "synthetic-service-token"}, 65536))
		defer server.Close()
		status, b := v3Call(t, server.Client(), server.URL+"/v3/streams/stream-1/audio/opus", v3Header(t, r), v3PacketBody(t))
		if status != http.StatusAccepted {
			t.Fatalf("HTTP %d %s", status, b)
		}
		response, check := lifecycle.DecodeRecord(b, p.Snapshot().IngressProfile)
		if response.Inspection == nil || check.Kind != lifecycle.Unknown {
			t.Fatalf("typed pending local result: %+v %s", check, b)
		}
		facts := map[lifecycle.SubBoundary]lifecycle.BoundaryObservation{}
		for _, o := range response.Inspection.Observations.Observations {
			facts[o.Address.SubBoundary] = o
		}
		for _, sub := range []lifecycle.SubBoundary{lifecycle.RawWrite, lifecycle.Durability, lifecycle.QueueAdmission} {
			if facts[sub].Outcome.Value == nil || *facts[sub].Outcome.Value != lifecycle.SuccessAtBoundary {
				t.Fatalf("real boundary missing %s: %+v", sub, facts[sub])
			}
		}
		if facts[lifecycle.QueueTerminal].Outcome.Value == nil || *facts[lifecycle.QueueTerminal].Outcome.Value != lifecycle.Pending || response.Inspection.Observations.Gate != lifecycle.Held {
			t.Fatal("queue admission became terminal success")
		}
		layout, e := archive.NewLayout(root, "stream-1")
		if e != nil {
			t.Fatal(e)
		}
		raw, e := os.ReadFile(layout.TmpDiscordOpus())
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Count(raw, []byte("\n")) != 1 || q.Pending() != 1 {
			t.Fatalf("real append/queue not reached: %q pending=%d", raw, q.Pending())
		}
		if bytes.Contains(b, []byte("AQID")) || bytes.Contains(b, []byte("opus_base64")) {
			t.Fatal("raw packet leaked into lifecycle response")
		}
		status, _ = v3Call(t, server.Client(), server.URL+"/v3/streams/stream-1/audio/opus", v3Header(t, r), v3PacketBody(t))
		if status != http.StatusConflict {
			t.Fatalf("repeated POST did not require same-attempt inspect: %d", status)
		}
		status, inspected := v3Call(t, server.Client(), server.URL+"/v3/streams/stream-1/audio/opus/inspect", v3Header(t, r), nil)
		if status != http.StatusAccepted || !bytes.Equal(bytes.TrimSpace(b), bytes.TrimSpace(inspected)) {
			t.Fatalf("inspect lost immutable receipt: %d %s", status, inspected)
		}
		after, e := os.ReadFile(layout.TmpDiscordOpus())
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(raw, after) || q.Pending() != 1 {
			t.Fatal("same-attempt inspect appended or requeued")
		}
	})
	t.Run("http_ack_loss_then_inspect_no_resend", func(t *testing.T) {
		p, m, q, r, root := v3HTTPFixture(t)
		handler := NewV3AudioIngressHandler(m, p, TokenVerifier{DiscordAudioPlainToken: "synthetic-service-token"}, 65536)
		dropped := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Header.Get("X-Synthetic-Drop-ACK") == "1" {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, request)
				conn, _, e := w.(http.Hijacker).Hijack()
				if e == nil {
					_ = conn.Close()
				}
				close(dropped)
				return
			}
			handler.ServeHTTP(w, request)
		}))
		defer server.Close()
		request, e := http.NewRequest(http.MethodPost, server.URL+"/v3/streams/stream-1/audio/opus", bytes.NewReader(v3PacketBody(t)))
		if e != nil {
			t.Fatal(e)
		}
		request.Header.Set("Authorization", "Bearer synthetic-service-token")
		request.Header.Set(LifecycleMetadataHeader, v3Header(t, r))
		request.Header.Set("X-Synthetic-Drop-ACK", "1")
		if response, e := server.Client().Do(request); e == nil {
			response.Body.Close()
			t.Fatal("fixture failed to lose HTTP ACK")
		}
		<-dropped
		status, b := v3Call(t, server.Client(), server.URL+"/v3/streams/stream-1/audio/opus/inspect", v3Header(t, r), nil)
		if status != http.StatusAccepted {
			t.Fatalf("inspect HTTP %d %s", status, b)
		}
		if q.Pending() != 1 {
			t.Fatal("HTTP ACK loss requeued or lost accepted work")
		}
		layout, _ := archive.NewLayout(root, "stream-1")
		raw, e := os.ReadFile(layout.TmpDiscordOpus())
		if e != nil || bytes.Count(raw, []byte("\n")) != 1 {
			t.Fatal("ACK loss replayed raw append")
		}
	})
}
