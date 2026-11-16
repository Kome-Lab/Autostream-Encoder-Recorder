package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/manualhold"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func msrTargetProducer(component lifecycle.ComponentID, serviceID string) lifecycle.ProducerRef {
	return lifecycle.ProducerRef{ComponentID: component, ServiceID: lifecycle.ID(serviceID), HostID: "synthetic-target", BootID: "synthetic-boot", ProcessIncarnation: lifecycle.ID("synthetic-process-" + strconv.Itoa(os.Getpid())), WriterID: "test-only-no-authority"}
}
func msrTargetRecord(t *testing.T, producer lifecycle.ProducerRef, streamID string, revision uint64, request, operation string) lifecycle.Record {
	t.Helper()
	resource := lifecycle.ResourceRef{Kind: lifecycle.Stream, OwnerID: producer.ServiceID, ResourceID: lifecycle.ID(streamID)}
	ref := lifecycle.ContractRef{ContractID: lifecycle.CC02, LogicalRevision: 1, SpecRef: lifecycle.SpecRef, ModuleCommitRef: manualhold.ModuleCommit}
	record := lifecycle.Record{Header: lifecycle.Header{ContractRef: ref, RecordKey: lifecycle.RecordKey{ContractRef: ref, OwnerID: "synthetic-cp", RecordID: lifecycle.ID(request)}, Correlation: lifecycle.Correlation{RequestID: lifecycle.ID(request), OperationID: lifecycle.ID(operation), AttemptID: lifecycle.ID("attempt-" + request), StepID: "start", ExecutionKey: lifecycle.ID("execution-" + request)}, DomainRef: lifecycle.DomainRef{DeploymentID: "synthetic-deployment", Realm: "synthetic-realm", DomainID: "synthetic-domain", MembershipRevision: 1, SnapshotRef: "membership-test", Resources: []lifecycle.ResourceRef{resource}}, ProducerRef: msrTargetProducer("C01", "synthetic-cp"), RecordRevision: revision, ProfileRef: lifecycle.ProfileRef{ProfileID: lifecycle.ExplicitResume, Revision: 1}}, Vector: &lifecycle.VectorRecord{}}
	for _, axis := range []lifecycle.Axis{lifecycle.Membership, lifecycle.Admission, lifecycle.Ownership, lifecycle.Fence, lifecycle.Boot, lifecycle.Process, lifecycle.RunAxis, lifecycle.ConfigAxis} {
		literal := lifecycle.ID("observed-" + string(axis))
		value := lifecycle.VersionedValue{Kind: lifecycle.PresentLiteral, Literal: &literal}
		if axis == lifecycle.Membership {
			v := uint64(1)
			value = lifecycle.VersionedValue{Kind: lifecycle.PresentUint64, Uint64: &v}
		}
		if axis == lifecycle.RunAxis {
			v := lifecycle.ID("run-test")
			value.Literal = &v
		}
		if axis == lifecycle.Boot {
			v := producer.BootID
			value.Literal = &v
		}
		if axis == lifecycle.Process {
			v := producer.ProcessIncarnation
			value.Literal = &v
		}
		record.Vector.Vector = append(record.Vector.Vector, lifecycle.RevisionEntry{Key: lifecycle.AxisKey{Axis: axis, OwnerID: resource.OwnerID, Resource: resource}, Incarnation: "synthetic-owner", Value: value, SourceFrameRef: "synthetic-cp-frame"})
	}
	if err := manualhold.Validate(record, streamID); err != nil {
		t.Fatal(err)
	}
	return record
}
func msrTargetGuard(t *testing.T, directory string, producer lifecycle.ProducerRef, inspector manualhold.Inspector) *manualhold.Guard {
	t.Helper()
	guard, err := manualhold.New(directory, inspector, func() (lifecycle.ProducerRef, error) { return producer, nil })
	if err != nil {
		t.Fatal(err)
	}
	guard.Require = func() bool { return true }
	return guard
}
func msrTargetRequest(t *testing.T, baseURL, route, token, body string, record *lifecycle.Record) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if record != nil {
		encoded, err := manualhold.Encode(*record)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(manualhold.Header, encoded)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

var msrLaunchMu sync.Mutex

func msrSyntheticLaunch(ctx context.Context) error {
	if barrier := os.Getenv("AUTOSTREAM_MSR_LAUNCH_BARRIER_FILE"); barrier != "" {
		if err := os.WriteFile(barrier+".reached", []byte("actual-launch-boundary"), 0600); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(barrier + ".release"); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	file := os.Getenv("AUTOSTREAM_MSR_START_COUNT_FILE")
	if file == "" {
		return nil
	}
	msrLaunchMu.Lock()
	defer msrLaunchMu.Unlock()
	var count int
	if data, err := os.ReadFile(file); err == nil {
		count, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(count + 1))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func msrServeProcess(t *testing.T, handler http.Handler) {
	t.Helper()
	listen := os.Getenv("AUTOSTREAM_MSR_LISTEN")
	ready := os.Getenv("AUTOSTREAM_MSR_READY_FILE")
	if listen == "" || ready == "" {
		t.Fatal("explicit synthetic listener and ready file required")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.WriteFile(ready, []byte("http://"+listener.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	server := http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second}
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

type msrActualStarter struct{ calls atomic.Int32 }

func (s *msrActualStarter) Start(ctx context.Context, _ string, _ []string) (streamproc.RunningProcess, error) {
	if err := msrSyntheticLaunch(ctx); err != nil {
		return nil, err
	}
	s.calls.Add(1)
	return &msrActualProcess{done: make(chan struct{})}, nil
}

type msrActualProcess struct {
	done chan struct{}
	once sync.Once
}

func (p *msrActualProcess) PID() int         { return 1 }
func (p *msrActualProcess) Wait() error      { <-p.done; return nil }
func (p *msrActualProcess) Stderr() string   { return "" }
func (p *msrActualProcess) Terminate() error { p.once.Do(func() { close(p.done) }); return nil }
func (p *msrActualProcess) Kill() error      { return p.Terminate() }
func msrEncoderHTTP(t *testing.T, guard *manualhold.Guard, starter *msrActualStarter, token string) http.Handler {
	t.Setenv("AUTOSTREAM_NODE_CONFIG", filepath.Join(t.TempDir(), "synthetic-pending-node.yml"))
	profile := ffmpeg.DefaultProfile()
	profile.Width = 16
	profile.Height = 16
	profile.FPS = 2
	manager := &streamproc.Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "synthetic-no-exec", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect, Profile: profile, StartAdmission: guard}
	runtimeConfig := func(ctx context.Context) (control.RuntimeConfig, error) {
		r := manualhold.RecordFromContext(ctx)
		if r == nil {
			return control.RuntimeConfig{}, errors.New("explicit synthetic stream required")
		}
		resource, err := manualhold.Resource(*r)
		if err != nil {
			return control.RuntimeConfig{}, err
		}
		return testYouTubeRuntimeProvider(string(resource.ResourceID))(ctx)
	}
	return NewServerWithManagersAndRuntimeConfig("encoder_recorder", manager, workerevents.NewManager(manager.ArchiveRoot), TokenVerifier{PlainToken: token}, testYouTubeSecretResolver, runtimeConfig)
}

const msrEncoderStartBody = "{\"stream_id\":\"stream-test\",\"archive_run_id\":\"run-test\",\"started_at\":\"2026-11-16T00:00:00Z\",\"name\":\"Synthetic MSR\",\"input_url\":\"srt://input.example.com:9000\"}"

func TestMSREncoderActualHTTPFence(t *testing.T) {
	t.Run("stop_waits_for_accepted_start", func(t *testing.T) {
		barrier := filepath.Join(t.TempDir(), "launch")
		t.Setenv("AUTOSTREAM_MSR_LAUNCH_BARRIER_FILE", barrier)
		producer := msrTargetProducer("C04", "encoder-test")
		guard := msrTargetGuard(t, t.TempDir(), producer, manualhold.InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) { return r, nil }))
		starter := &msrActualStarter{}
		product := msrEncoderHTTP(t, guard, starter, "synthetic-service-token")
		stopArrived := make(chan struct{})
		var once sync.Once
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/streams/stream-test/stop" {
				once.Do(func() { close(stopArrived) })
			}
			product.ServeHTTP(w, r)
		}))
		defer server.Close()
		defer func() { _ = os.WriteFile(barrier+".release", []byte("release"), 0600) }()
		start := msrTargetRecord(t, producer, "stream-test", 1, "accepted-before-stop", "automatic-start")
		starting := msrAsyncTargetRequest(t, server.URL, "/streams/start", msrEncoderStartBody, &start)
		msrAwaitActualLaunchBarrier(t, barrier)
		stopping := msrAsyncTargetRequest(t, server.URL, "/streams/stream-test/stop", "{}", nil)
		select {
		case <-stopArrived:
		case <-time.After(5 * time.Second):
			t.Fatal("actual Stop handler was not reached")
		}
		select {
		case result := <-stopping:
			t.Fatalf("Stop returned before accepted Start left actual Starter boundary: %d %s %v", result.code, result.body, result.err)
		case <-time.After(30 * time.Millisecond):
		}
		if err := os.WriteFile(barrier+".release", []byte("release"), 0600); err != nil {
			t.Fatal(err)
		}
		started := <-starting
		if started.err != nil || started.code != http.StatusAccepted {
			t.Fatalf("Start completion: %d %s %v", started.code, started.body, started.err)
		}
		stopped := <-stopping
		if stopped.err != nil || stopped.code != http.StatusAccepted || !strings.Contains(string(stopped.body), "stopping") || starter.calls.Load() != 1 {
			t.Fatalf("Stop after Start completion: %d %s %v launches=%d", stopped.code, stopped.body, stopped.err, starter.calls.Load())
		}
		// The existing Stop response retains its captured "stopping" snapshot.
		// Observe terminal state through the existing pure status API separately.
		deadline := time.Now().Add(5 * time.Second)
		for {
			request, err := http.NewRequest(http.MethodGet, server.URL+"/streams/stream-test/process-status", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer synthetic-service-token")
			response, err := (&http.Client{Timeout: time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var current processSnapshotResponse
			decodeErr := json.NewDecoder(response.Body).Decode(&current)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil {
				t.Fatalf("pure status after Stop: %d %v", response.StatusCode, decodeErr)
			}
			if current.Status == "stopped" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Stop terminal state not observed: %s", current.Status)
			}
			time.Sleep(5 * time.Millisecond)
		}
		code, body := msrTargetRequest(t, server.URL, "/streams/stream-test/stop", "synthetic-service-token", "{}", nil)
		if code != http.StatusAccepted || !strings.Contains(string(body), "already_stopped") || starter.calls.Load() != 1 {
			t.Fatalf("late Start passed Stop ACK or stop receipt changed: %d %s launches=%d", code, body, starter.calls.Load())
		}
	})

	producer := msrTargetProducer("C04", "encoder-test")
	var checks atomic.Int32
	inspector := manualhold.InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) { checks.Add(1); return r, nil })
	guard := msrTargetGuard(t, t.TempDir(), producer, inspector)
	starter := &msrActualStarter{}
	server := httptest.NewServer(msrEncoderHTTP(t, guard, starter, "synthetic-service-token"))
	defer server.Close()
	start := msrTargetRecord(t, producer, "stream-test", 1, "old-start", "automatic-start")
	closeRecord := msrTargetRecord(t, producer, "stream-test", 2, "manual-stop", "manual-stop")
	raw, _ := json.Marshal(closeRecord)
	code, body := msrTargetRequest(t, server.URL, "/streams/stream-test/start-admission/close", "synthetic-service-token", string(raw), nil)
	if code != 200 {
		t.Fatalf("close %d %s", code, body)
	}
	code, body = msrTargetRequest(t, server.URL, "/streams/start", "synthetic-service-token", msrEncoderStartBody, &start)
	if code != 409 || starter.calls.Load() != 0 {
		t.Fatalf("delayed actual Start: %d %s launches=%d", code, body, starter.calls.Load())
	}
	manual := msrTargetRecord(t, producer, "stream-test", 2, "manual-start", "manual-start")
	code, body = msrTargetRequest(t, server.URL, "/streams/start", "synthetic-service-token", msrEncoderStartBody, &manual)
	if code != 202 || starter.calls.Load() != 1 || checks.Load() < 2 {
		t.Fatalf("manual actual Start: %d %s launches=%d checks=%d", code, body, starter.calls.Load(), checks.Load())
	}
	changed := strings.Replace(msrEncoderStartBody, "Synthetic MSR", "Changed private request", 1)
	code, body = msrTargetRequest(t, server.URL, "/streams/start", "synthetic-service-token", changed, &manual)
	if code != 409 || starter.calls.Load() != 1 {
		t.Fatalf("same request changed body replay: %d %s", code, body)
	}
	if managed, err := guard.Managed("stream-test"); err != nil || !managed {
		t.Fatalf("manual Start lost Hold: %v", err)
	}
	_, _ = msrTargetRequest(t, server.URL, "/streams/stream-test/stop", "synthetic-service-token", "{}", nil)
}
func TestMSREncoderActualPrelaunchCurrentness(t *testing.T) {
	uncertain := httptest.NewRecorder()
	writeStartAdmissionError(uncertain, errors.Join(manualhold.ErrRefused, manualhold.ErrUnknown))
	if uncertain.Code != http.StatusServiceUnavailable {
		t.Fatal("UNKNOWN persistence result became refused unchanged")
	}
	producer := msrTargetProducer("C04", "encoder-test")
	var checks atomic.Int32
	inspector := manualhold.InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) {
		if checks.Add(1) > 1 {
			return lifecycle.Record{}, manualhold.ErrRefused
		}
		return r, nil
	})
	guard := msrTargetGuard(t, t.TempDir(), producer, inspector)
	starter := &msrActualStarter{}
	server := httptest.NewServer(msrEncoderHTTP(t, guard, starter, "synthetic-service-token"))
	defer server.Close()
	start := msrTargetRecord(t, producer, "stream-test", 1, "start", "automatic-start")
	code, body := msrTargetRequest(t, server.URL, "/streams/start", "synthetic-service-token", msrEncoderStartBody, &start)
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "UNKNOWN") || starter.calls.Load() != 0 || checks.Load() != 2 {
		t.Fatalf("prelaunch fence: %d %s launches=%d checks=%d", code, body, starter.calls.Load(), checks.Load())
	}
	observed, observation, inspectErr := guard.Inspect("stream-test", start.Header.Correlation.RequestID)
	if inspectErr != nil || !manualhold.Equal(observed, start) || observation == nil || observation.Knowledge != lifecycle.KnowledgeUnknown || observation.Outcome.State != lifecycle.Unavailable || observation.Outcome.Value != nil {
		t.Fatalf("late product-preparation rejection must remain UNKNOWN: %v %+v", inspectErr, observation)
	}
}
func TestMSRTargetProcess(t *testing.T) {
	if os.Getenv("AUTOSTREAM_MSR_TARGET_HELPER") != "1" {
		return
	}
	service := os.Getenv("AUTOSTREAM_MSR_SERVICE_ID")
	token := os.Getenv("AUTOSTREAM_MSR_TOKEN")
	directory := os.Getenv("AUTOSTREAM_MSR_GUARD_DIR")
	baseURL := os.Getenv("AUTOSTREAM_MSR_CP_URL")
	if service == "" || token == "" || directory == "" || baseURL == "" || !filepath.IsAbs(directory) {
		t.Fatal("explicit synthetic target configuration required")
	}
	inspector := manualhold.HTTPInspector{Config: func() (string, string, error) { return baseURL, token, nil }, HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	guard := msrTargetGuard(t, directory, msrTargetProducer("C04", service), inspector)
	msrServeProcess(t, msrEncoderHTTP(t, guard, &msrActualStarter{}, token))
}

type msrHTTPResult struct {
	code int
	body []byte
	err  error
}

func msrAsyncTargetRequest(t *testing.T, baseURL, route, body string, record *lifecycle.Record) <-chan msrHTTPResult {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-service-token")
	request.Header.Set("Content-Type", "application/json")
	if record != nil {
		encoded, err := manualhold.Encode(*record)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set(manualhold.Header, encoded)
	}
	result := make(chan msrHTTPResult, 1)
	go func() {
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			result <- msrHTTPResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		result <- msrHTTPResult{code: response.StatusCode, body: body, err: err}
	}()
	return result
}
func msrAwaitActualLaunchBarrier(t *testing.T, barrier string) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(barrier + ".reached"); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-timeout.C:
			t.Fatal("actual launch boundary not reached")
		case <-ticker.C:
		}
	}
}
