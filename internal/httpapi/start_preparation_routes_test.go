package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/example/autostream-contracts/pkg/contracts"
	fixtures "github.com/example/autostream-contracts/tests"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type preparationHTTPProcess struct {
	once sync.Once
	done chan struct{}
}

func (p *preparationHTTPProcess) PID() int         { return 4321 }
func (p *preparationHTTPProcess) Wait() error      { <-p.done; return nil }
func (p *preparationHTTPProcess) Kill() error      { p.once.Do(func() { close(p.done) }); return nil }
func (p *preparationHTTPProcess) Terminate() error { return p.Kill() }

type preparationHTTPStarter struct {
	count atomic.Int32
	args  []string
}

func (s *preparationHTTPStarter) Start(_ context.Context, _ string, args []string) (streamproc.RunningProcess, error) {
	s.count.Add(1)
	s.args = append([]string(nil), args...)
	return &preparationHTTPProcess{done: make(chan struct{})}, nil
}

type preparationHTTPFixture struct {
	handler  http.Handler
	pm       *streamproc.Manager
	starter  *preparationHTTPStarter
	witness  *httpCoverWitness
	request  map[string]any
	identity contracts.EncoderStartPreparationIdentity
	resolves atomic.Int32
	root     string
}

func newPreparationHTTPFixture(t *testing.T) *preparationHTTPFixture {
	t.Helper()
	t.Setenv("AUTOSTREAM_ENV", "development")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR", "127.0.0.1:0")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST", "127.0.0.1")
	f := &preparationHTTPFixture{root: t.TempDir(), starter: &preparationHTTPStarter{}, witness: &httpCoverWitness{}}
	var corpus map[string]json.RawMessage
	_ = json.Unmarshal([]byte(fixtures.StartPreparationJSON), &corpus)
	_ = json.Unmarshal(corpus["prepare"], &f.request)
	f.request["encoder_service_id"] = "encoder-recorder-01"
	nested := f.request["start_request"].(map[string]any)
	token, err := ingesttoken.Issue("node-config-signing-key", ingesttoken.Claims{StreamID: "stream-01", ServiceID: "worker-01", ServiceType: "worker", Purpose: "worker_video", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	nested["worker_video_ingest_token"] = token
	f.identity = contracts.EncoderStartPreparationIdentity{StreamID: "stream-01", StartID: f.request["start_id"].(string), EncoderServiceID: "encoder-recorder-01", JobGeneration: 7, ArchiveRunID: "archive-01"}
	f.pm = &streamproc.Manager{ArchiveRoot: f.root, Starter: f.starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: "direct", CoverAssets: &videocover.Loader{}, CoverWitness: f.witness}
	provider := func(ctx context.Context) (control.RuntimeConfig, error) {
		cfg, e := testYouTubeRuntimeProvider("stream-01")(ctx)
		cfg.Profiles = map[string][]control.RuntimeProfile{"encoder": {{ID: "profile-01", Config: map[string]any{"width": 128, "height": 72, "fps": 15}}}}
		return cfg, e
	}
	resolver := func(context.Context, string, string, string) (string, error) {
		f.resolves.Add(1)
		return "synthetic-private-output", nil
	}
	f.handler = NewServerWithManagersAndRuntimeConfig("encoder_recorder", f.pm, workerevents.NewManager(f.root), TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: "node-config-signing-key", RequireSignedIngest: true}, resolver, provider)
	t.Cleanup(func() {
		_, _ = f.pm.AbortStartPreparation(context.Background(), streamproc.StartPreparationIdentity{StreamID: f.identity.StreamID, StartID: f.identity.StartID, EncoderServiceID: f.identity.EncoderServiceID, JobGeneration: 7})
	})
	return f
}
func (f *preparationHTTPFixture) call(method, path string, value any, token string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(value)
	if raw, ok := value.([]byte); ok {
		b = raw
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func (f *preparationHTTPFixture) base() string {
	return "/streams/stream-01/start-preparations/" + f.identity.StartID
}
func (f *preparationHTTPFixture) action() map[string]any {
	return map[string]any{"schema_version": 2, "stream_id": "stream-01", "start_id": f.identity.StartID, "encoder_service_id": f.identity.EncoderServiceID, "job_generation": 7}
}
func TestStartPreparationHTTPPrepareCommitAbortAndSecretBoundary(t *testing.T) {
	f := newPreparationHTTPFixture(t)
	w := f.call("POST", "/streams/start-preparations", f.request, "service-token")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	prepared, err := contracts.DecodeEncoderStartPreparationPrepared(w.Body.Bytes(), f.identity)
	if err != nil {
		t.Fatal(err)
	}
	if f.witness.calls != 0 || f.starter.count.Load() != 1 || f.resolves.Load() != 1 {
		t.Fatal("prepare waited witness or repeated side effects")
	}
	if duplicate := f.call("POST", "/streams/start-preparations", f.request, "service-token"); duplicate.Code != 409 || f.starter.count.Load() != 1 || f.resolves.Load() != 1 {
		t.Fatal("duplicate consumed secret lease/spawn")
	}
	for _, phase := range []string{"prepared", "running", "aborted"} {
		if phase != "prepared" {
			action := "commit"
			if phase == "aborted" {
				action = "abort"
			}
			w = f.call("POST", f.base()+"/"+action, f.action(), "service-token")
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		w = f.call("GET", f.base(), nil, "service-token")
		state, e := contracts.DecodeEncoderStartPreparationStatus(w.Body.Bytes(), f.identity)
		if w.Code != 200 || e != nil || state.Phase != phase {
			t.Fatal(w.Code, e, state.Phase, w.Body.String())
		}
		for _, private := range []string{prepared.VideoIngest.Passphrase, "synthetic-private-output", "video_ingest", "worker_video_ingest_token"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("private material in GET")
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable preparation status")
		}
	}
	if f.witness.calls != 1 || f.resolves.Load() != 1 {
		t.Fatal("commit reused lease or witness")
	}
	joined := strings.Join(f.starter.args, " ")
	for _, want := range []string{"-f mjpeg", "tcp://127.0.0.1:", "discord-opus.sdp", "-map [v] -map [aout_stats]"} {
		if !strings.Contains(joined, want) {
			t.Fatal("managed pipeline arg missing", want)
		}
	}
	if strings.Contains(joined, prepared.VideoIngest.Passphrase) || strings.Contains(joined, f.request["start_request"].(map[string]any)["worker_video_ingest_token"].(string)) {
		t.Fatal("route credential in FFmpeg argv")
	}
	metadata, e := os.ReadFile(filepath.Join(f.root, "tmp", "stream-01", "metadata.json"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(metadata, []byte(prepared.VideoIngest.Passphrase)) || bytes.Contains(metadata, []byte("synthetic-private-output")) {
		t.Fatal("secret in metadata")
	}
}
func TestStartPreparationHTTPRejectsBeforeResources(t *testing.T) {
	for _, kind := range []string{"token", "wrong_node", "wrong_type", "body_type", "extra", "nested_extra", "bad_ingest", "cross_stream_ingest", "oversize", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparationHTTPFixture(t)
			token := "service-token"
			path := "/streams/start-preparations"
			want := 400
			var body any = f.request
			switch kind {
			case "token":
				token = "wrong"
				want = 401
			case "wrong_node":
				f.request["encoder_service_id"] = "another-encoder"
				want = 409
			case "wrong_type":
				p := filepath.Join(t.TempDir(), "config.yml")
				writeNodeConfigForVerifierTest(t, p, "worker")
				t.Setenv("AUTOSTREAM_NODE_CONFIG", p)
				want = 409
			case "body_type":
				f.request["schema_version"] = "2"
			case "extra":
				f.request["extra"] = true
			case "nested_extra":
				f.request["start_request"].(map[string]any)["extra"] = true
			case "bad_ingest":
				f.request["start_request"].(map[string]any)["worker_video_ingest_token"] = "invalid"
				want = 401
			case "cross_stream_ingest":
				f.request["start_request"].(map[string]any)["stream_id"] = "different-stream"
				want = 401
			case "oversize":
				body = []byte(`{}` + strings.Repeat(" ", maxControlBodyBytes))
				want = 413
			case "legacy":
				path = "/streams/start"
				body = f.request["start_request"]
				want = 409
			}
			w := f.call("POST", path, body, token)
			if w.Code != want {
				t.Fatal(w.Code, want, w.Body.String())
			}
			if f.starter.count.Load() != 0 || f.resolves.Load() != 0 {
				t.Fatal("rejection allocated resource or consumed lease")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("error cacheable")
			}
		})
	}
}
func TestStartPreparationHTTPActionsExactOwner(t *testing.T) {
	f := newPreparationHTTPFixture(t)
	if w := f.call("POST", "/streams/start-preparations", f.request, "service-token"); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	for _, kind := range []string{"unknown", "stream", "generation", "node", "missing", "extra", "token"} {
		t.Run(kind, func(t *testing.T) {
			a := f.action()
			path := f.base() + "/commit"
			token := "service-token"
			want := 400
			switch kind {
			case "unknown":
				a["start_id"] = "22222222-2222-4222-8222-222222222222"
				path = "/streams/stream-01/start-preparations/" + a["start_id"].(string) + "/commit"
				want = 404
			case "stream":
				a["stream_id"] = "other"
			case "generation":
				a["job_generation"] = 8
				want = 409
			case "node":
				a["encoder_service_id"] = "another"
				want = 409
			case "missing":
				delete(a, "job_generation")
			case "extra":
				a["force"] = true
			case "token":
				token = "wrong"
				want = 401
			}
			w := f.call("POST", path, a, token)
			if w.Code != want {
				t.Fatal(w.Code, want, w.Body.String())
			}
			if f.witness.calls != 0 || f.starter.count.Load() != 1 {
				t.Fatal("bad action advanced runtime")
			}
		})
	}
	w := f.call("POST", f.base()+"/abort", f.action(), "service-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
