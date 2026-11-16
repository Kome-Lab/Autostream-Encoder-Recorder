//go:build iu_crossrepo

package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/lifecycleparticipant"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

// This helper runs only in a separately compiled test process. It has no normal
// startup path. The consumer test drives the actual HTTP handler, archive file
// append and bridge queue; only authentication, owner state and queue consumer
// are isolated fixtures. No decoder, network media sink or provider is started.
func TestIUWS3CrossRepositoryEncoderProcess(t *testing.T) {
	source := os.Getenv("IU_WS3_ER_SOURCE")
	shared := os.Getenv("IU_WS3_FIXTURE_DIR")
	if !filepath.IsAbs(source) || !filepath.IsAbs(shared) {
		t.Fatal("explicit absolute isolated fixture paths required")
	}
	data, err := os.ReadFile(filepath.Join(source, "internal/lifecycleparticipant/testdata/owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var owner lifecycleparticipant.OwnerSnapshot
	if err = json.Unmarshal(data, &owner); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	participant, err := lifecycleparticipant.NewSyntheticParticipant(lifecycleparticipant.SyntheticConfig{
		Environment: "ISOLATED_TEST_ONLY", JournalDir: filepath.Join(root, "journal"), Current: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if participant.ProductionQualified() {
		t.Fatal("synthetic participant qualified production")
	}
	manager, queue, err := audioingest.NewSyntheticQueueFixture(filepath.Join(root, "archive"), owner.StreamID, 8)
	if err != nil {
		t.Fatal(err)
	}
	product := NewV3AudioIngressHandler(manager, participant, TokenVerifier{
		DiscordAudioPlainToken: "synthetic-only-not-a-credential", RequireSignedIngest: false,
	}, 65536)
	metadata, err := os.ReadFile(filepath.Join(source, "internal/lifecycleparticipant/testdata/request.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureBody, err := json.Marshal(audioingest.IngestRequest{
		StreamID: owner.StreamID, Source: string(owner.IngressProducer.ServiceID),
		Packets: []audioingest.Packet{{SSRC: 1, Sequence: 1, Timestamp: 960,
			ReceivedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), OpusBase64: "AQID"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var fixtureMu sync.Mutex
	queueConsumed := false
	var posts, inspections atomic.Int64
	var workerPosts atomic.Int64
	const workerStream = "stream-01"
	workerRoot := filepath.Join(root, "worker-archive")
	workerEvents := workerevents.NewManager(workerRoot)
	processes := &streamproc.Manager{ArchiveRoot: workerRoot, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{},
		InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	workerProduct := newV2TestServerWithManagers(t, processes, workerEvents, TokenVerifier{
		PlainToken: "synthetic-control-only-token", WorkerEventsPlainToken: "synthetic-only-not-a-credential",
		IngestTokenSigningKey: "synthetic-unused-worker-signing-material", RequireSignedIngest: false,
	}, workerStream)
	start := httptest.NewRequest(http.MethodPost, "/streams/start", strings.NewReader(`{"stream_id":"`+workerStream+`","name":"Isolated Worker Fixture","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	start.Header.Set("Authorization", "Bearer synthetic-control-only-token")
	started := httptest.NewRecorder()
	workerProduct.ServeHTTP(started, start)
	if started.Code != http.StatusAccepted {
		t.Fatalf("synthetic process fixture did not start: %d %s", started.Code, started.Body.String())
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fixture/worker-state":
			layout, e := archive.NewLayout(workerRoot, workerStream)
			if e != nil {
				http.Error(w, "worker fixture layout unavailable", http.StatusInternalServerError)
				return
			}
			read := func(name string) []byte {
				data, e := os.ReadFile(filepath.Join(layout.TmpDir(), name))
				if e != nil && !os.IsNotExist(e) {
					return nil
				}
				return data
			}
			logs := read("logs.jsonl")
			captions := read("captions.vtt")
			var transcript []json.RawMessage
			_ = json.Unmarshal(read("transcript.json"), &transcript)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Posts": workerPosts.Load(), "LogLines": strings.Count(string(logs), "\n"),
				"CaptionCues": strings.Count(string(captions), " --> "), "TranscriptEntries": len(transcript),
				"ProductionQualified": false,
			})
			return
		case "/fixture/closure/feed":
			fixtureMu.Lock()
			defer fixtureMu.Unlock()
			request, e := http.NewRequestWithContext(r.Context(), http.MethodPost, "/v3/streams/"+owner.StreamID+"/audio/opus", bytes.NewReader(fixtureBody))
			if e != nil {
				http.Error(w, "fixture request unavailable", http.StatusInternalServerError)
				return
			}
			request.Header.Set("Authorization", "Bearer synthetic-only-not-a-credential")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(LifecycleMetadataHeader, base64.RawURLEncoding.EncodeToString(metadata))
			posts.Add(1)
			product.ServeHTTP(w, request)
			return
		case "/fixture/closure/close":
			_ = json.NewEncoder(w).Encode(participant.CloseAdmission(r.Context()))
			return
		case "/fixture/closure/dequeue":
			fixtureMu.Lock()
			defer fixtureMu.Unlock()
			if queueConsumed || !queue.ConsumeOne() {
				http.Error(w, "fixture queue has no accepted item", http.StatusConflict)
				return
			}
			queueConsumed = true
			w.WriteHeader(http.StatusNoContent)
			return
		case "/fixture/closure/terminal":
			fixtureMu.Lock()
			defer fixtureMu.Unlock()
			if !queueConsumed {
				http.Error(w, "fixture queue duty remains", http.StatusConflict)
				return
			}
			original, _ := participant.Inspect(owner.StreamID, metadata)
			if original.Inspection == nil {
				http.Error(w, "original fixture attempt unavailable", http.StatusConflict)
				return
			}
			// The explicit synthetic owner records a fresh local safety-release
			// callback only after the real queue consumer has dequeued the item.
			// This does not establish a codec, remote terminal result or READY.
			witness := lifecycle.ID("fixture-local-queue-release")
			terminal := lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &witness,
				Observation: &lifecycle.ObservationContext{OwnerID: owner.Frame.OwnerID, Scope: owner.Frame.Scope,
					FrameRef: "encoder-terminal-frame-2", SourceRevision: owner.Frame.SourceRevision + 1}}
			_ = json.NewEncoder(w).Encode(participant.ObserveQueueTerminal(owner.StreamID, metadata, original.Header.RecordRevision, terminal))
			return
		case "/fixture/closure/completion":
			completion, boundaries, assessment := participant.CompletionSnapshot()
			current := participant.Snapshot()
			_ = json.NewEncoder(w).Encode(struct {
				Profile             lifecycle.ComparisonProfile
				Membership          lifecycle.Record
				Admission           lifecycle.Record
				Completion          lifecycle.Record
				CurrentVector       lifecycle.RevisionVector
				Boundaries          lifecycle.ObservationSet
				Result              lifecycle.Result
				ProductionQualified bool
			}{current.ClosureProfile, current.Membership, participant.ClosureAdmission(), completion,
				current.ClosureVector, boundaries, assessment, participant.ProductionQualified()})
			return
		case "/fixture/state":
			layout, e := archive.NewLayout(manager.ArchiveRoot, owner.StreamID)
			if e != nil {
				http.Error(w, "fixture layout unavailable", http.StatusInternalServerError)
				return
			}
			raw, e := os.ReadFile(layout.TmpDiscordOpus())
			if e != nil && !os.IsNotExist(e) {
				http.Error(w, "fixture raw unavailable", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Posts": posts.Load(), "Inspections": inspections.Load(), "RawLines": strings.Count(string(raw), "\n"),
				"QueuePending": queue.Pending(), "ProductionQualified": participant.ProductionQualified(),
			})
			return
		case "/fixture/finish":
			w.WriteHeader(http.StatusNoContent)
			stopOnce.Do(func() { close(done) })
			return
		}
		if strings.HasPrefix(r.URL.Path, "/worker-normal/") {
			workerPosts.Add(1)
			http.StripPrefix("/worker-normal", workerProduct).ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/worker-ack-loss/") {
			workerPosts.Add(1)
			copyRequest := r.Clone(r.Context())
			copyURL := *r.URL
			copyURL.Path = strings.TrimPrefix(copyURL.Path, "/worker-ack-loss")
			copyRequest.URL = &copyURL
			workerProduct.ServeHTTP(httptest.NewRecorder(), copyRequest)
			connection, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				_ = connection.Close()
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/inspect") {
			inspections.Add(1)
		} else {
			posts.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/false-2xx/") {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ack-loss/") {
			copyRequest := r.Clone(r.Context())
			copyURL := *r.URL
			copyURL.Path = strings.TrimPrefix(copyURL.Path, "/ack-loss")
			copyRequest.URL = &copyURL
			product.ServeHTTP(httptest.NewRecorder(), copyRequest)
			connection, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				_ = connection.Close()
			}
			return
		}
		http.StripPrefix("/normal", product).ServeHTTP(w, r)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	info, err := os.OpenFile(filepath.Join(shared, "server.url"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = info.WriteString(server.URL); err != nil {
		_ = info.Close()
		t.Fatal(err)
	}
	if err = info.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("isolated consumer did not finish")
	}
}
