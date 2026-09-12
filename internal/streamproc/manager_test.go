package streamproc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
	"github.com/example/autostream-encoder-recorder/internal/watermarkfeed"
)

type coverTestFetcher struct {
	body  []byte
	meta  videocover.FetchMetadata
	calls int
	err   error
}

func (f *coverTestFetcher) Fetch(_ context.Context, _ videocover.AssetRef, _ int64) ([]byte, videocover.FetchMetadata, error) {
	f.calls++
	return append([]byte(nil), f.body...), f.meta, f.err
}

type coverTestWitness struct {
	calls    int
	failNext bool
}

type watermarkTestWitness struct {
	calls    int
	failNext bool
}

func (w *watermarkTestWitness) Apply(_ context.Context, source *watermarkfeed.Source, frame []byte, _ string) error {
	w.calls++
	if err := source.Update(frame); err != nil {
		return err
	}
	if w.failNext {
		w.failNext = false
		return errors.New("injected watermark witness timeout")
	}
	return nil
}

func (w *coverTestWitness) Apply(_ context.Context, source *imagefeed.Source, frame []byte, initial bool, _ string) error {
	w.calls++
	if !initial {
		if err := source.Update(frame); err != nil {
			return err
		}
	}
	if w.failNext {
		w.failNext = false
		return errors.New("injected graph witness timeout")
	}
	return nil
}

func coverPNGFixture(t *testing.T) ([]byte, videocover.MediaAssetDescriptor) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for y := 0; y < 9; y++ {
		for x := 0; x < 16; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var body bytes.Buffer
	if err := png.Encode(&body, img); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body.Bytes())
	ppm, opaque := 0, true
	descriptor := videocover.MediaAssetDescriptor{
		AssetID: "cover-asset", VariantID: "cover-variant", Usage: "video_cover", MediaType: "image/png",
		Width: 16, Height: 9, ByteSize: int64(body.Len()), PixelCount: 144, AspectRatioErrorPPM: &ppm,
		Opaque: &opaque, SHA256: hex.EncodeToString(sum[:]), Revision: 1, Readiness: videocover.ReadinessReady,
	}
	return body.Bytes(), descriptor
}

func coverTestManager(t *testing.T, starter *fakeStarter, fetcher *coverTestFetcher, witness *coverTestWitness) *Manager {
	t.Helper()
	profile := ffmpeg.DefaultProfile()
	profile.Width, profile.Height, profile.FPS = 16, 9, 2
	return &Manager{
		ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver,
		AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: staticRelayBindingID,
		Profile: profile, CoverAssets: videocover.NewLoader(fetcher, 4, 1<<20), CoverWitness: witness,
		WatermarkWitness:  &watermarkTestWitness{},
		CoverApplyTimeout: time.Second,
	}
}

func coverStartJob(descriptor *videocover.MediaAssetDescriptor) lifecycle.StreamJob {
	return lifecycle.StreamJob{
		StreamID: "stream-cover", Name: "Cover Stream", InputURL: "srt://input.example.com:9000",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true,
		VideoCoverStart: &videocover.StartSnapshot{JobGeneration: 7, Revision: 1, Active: descriptor != nil, IdempotencyKey: "start-cover-7", CoverAsset: descriptor},
	}
}

func zeroAudioContinuity(value videocover.VisualAudioContinuity) bool {
	return value == (videocover.VisualAudioContinuity{})
}

type recordingWriteCloser struct{ bytes.Buffer }

func (*recordingWriteCloser) Close() error { return nil }

const (
	staticRelayBindingID      = "relay-11111111-1111-1111-1111-111111111111"
	otherStaticRelayBindingID = "relay-22222222-2222-2222-2222-222222222222"
)

type fakeStarter struct {
	mu        sync.Mutex
	bin       string
	args      []string
	process   *fakeProcess
	processes []*fakeProcess
}

func (s *fakeStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bin = bin
	s.args = append([]string(nil), args...)
	s.process = &fakeProcess{done: make(chan error, 1)}
	s.processes = append(s.processes, s.process)
	return s.process, nil
}

type fakeProcess struct {
	done       chan error
	terminated bool
	killed     bool
	commands   []string
	stderr     string
}

func (p *fakeProcess) Command(target, command, argument string) error {
	p.commands = append(p.commands, target+" "+command+" "+argument)
	return nil
}

func (p *fakeProcess) PID() int {
	return 1234
}

func (p *fakeProcess) Wait() error {
	return <-p.done
}

func (p *fakeProcess) Stderr() string {
	return p.stderr
}

func (p *fakeProcess) Terminate() error {
	p.terminated = true
	p.done <- nil
	return nil
}

func (p *fakeProcess) Kill() error {
	p.killed = true
	p.done <- nil
	return nil
}

func testInputResolver(ctx context.Context, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

type blockingStarter struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	count   int
}

func (s *blockingStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.count++
	if s.count == 1 {
		close(s.started)
	}
	s.mu.Unlock()
	<-s.release
	return &fakeProcess{done: make(chan error, 1)}, nil
}

type fakeReporter struct {
	mu      sync.Mutex
	signals []observability.Signal
}

type fakePackager struct {
	mu                   sync.Mutex
	root                 string
	err                  error
	delay                time.Duration
	artifactsBeforeError bool
	jobs                 []lifecycle.PackageJob
}

type artifactReportCall struct {
	streamID   string
	artifacts  []control.Artifact
	archiveRun control.ArchiveRun
}

type fakeArtifactReporter struct {
	mu    sync.Mutex
	err   error
	calls []artifactReportCall
}

func (r *fakeArtifactReporter) ReportArtifacts(ctx context.Context, streamID string, archiveRun control.ArchiveRun, artifacts []control.Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, artifactReportCall{streamID: streamID, artifacts: append([]control.Artifact(nil), artifacts...), archiveRun: archiveRun})
	return r.err
}

func (r *fakeArtifactReporter) calledWithRun(streamID, archiveRunID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call.streamID == streamID && call.archiveRun.ID == archiveRunID {
			return true
		}
	}
	return false
}

func (r *fakeArtifactReporter) calledWith(streamID, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call.streamID != streamID {
			continue
		}
		expectedPath := "final/" + streamID + "/" + name
		if call.archiveRun.ID != "" {
			expectedPath = "final/" + streamID + "/" + call.archiveRun.ID + "/" + name
		}
		for _, artifact := range call.artifacts {
			if artifact.Name == name && artifact.RelativePath == expectedPath {
				return true
			}
		}
	}
	return false
}

func (r *fakeArtifactReporter) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (p *fakePackager) Package(ctx context.Context, job lifecycle.PackageJob) (lifecycle.Result, error) {
	if err := ctx.Err(); err != nil {
		return lifecycle.Result{}, err
	}
	p.mu.Lock()
	p.jobs = append(p.jobs, job)
	p.mu.Unlock()
	if p.delay > 0 {
		select {
		case <-ctx.Done():
			return lifecycle.Result{}, ctx.Err()
		case <-time.After(p.delay):
		}
	}
	if p.err != nil && !p.artifactsBeforeError {
		return lifecycle.Result{}, p.err
	}
	layout, err := archive.NewRunLayout(filepath.Join(p.root, "archives"), job.StreamID, job.ArchiveRunID)
	if err != nil {
		return lifecycle.Result{}, err
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		return lifecycle.Result{}, err
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("packaged video"), 0o640); err != nil {
		return lifecycle.Result{}, err
	}
	if err := os.WriteFile(layout.FinalMetadata(), []byte(`{"stream_id":"`+job.StreamID+`"}`+"\n"), 0o640); err != nil {
		return lifecycle.Result{}, err
	}
	if p.err != nil {
		return lifecycle.Result{}, p.err
	}
	return lifecycle.Result{
		Layout: layout,
		Metadata: lifecycle.Metadata{
			StreamID: job.StreamID,
			Name:     job.Name,
			Upload: archive.UploadResult{
				DryRun:   true,
				FolderID: "dry-run-folder",
				Attempts: 1,
				FileIDs:  map[string]string{"final.mp4": "dry-run-file", "metadata.json": "dry-run-file"},
			},
		},
		RemuxDurationMS: 125,
	}, nil
}

func (p *fakePackager) calledWith(streamID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, job := range p.jobs {
		if job.StreamID == streamID {
			return true
		}
	}
	return false
}

func (p *fakePackager) jobWith(streamID string) (lifecycle.PackageJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, job := range p.jobs {
		if job.StreamID == streamID {
			return job, true
		}
	}
	return lifecycle.PackageJob{}, false
}

func (r *fakeReporter) Report(ctx context.Context, signal observability.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, signal)
	return nil
}

func (r *fakeReporter) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name {
			return true
		}
	}
	return false
}

func (r *fakeReporter) hasValueAtLeast(name string, min float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name && signal.Value != nil && *signal.Value >= min {
			return true
		}
	}
	return false
}

func (r *fakeReporter) hasValue(name string, want float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name && signal.Value != nil && *signal.Value == want {
			return true
		}
	}
	return false
}

func (r *fakeReporter) find(name string) (observability.Signal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name {
			return signal, true
		}
	}
	return observability.Signal{}, false
}

func (r *fakeReporter) signalsSnapshot() []observability.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observability.Signal(nil), r.signals...)
}

func (r *fakeReporter) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.signals))
	for _, signal := range r.signals {
		out = append(out, signal.Name)
	}
	return out
}

func anyMapString(items map[string]any) string {
	out := ""
	for key, value := range items {
		out += key + "=" + valueString(value) + ";"
	}
	return out
}

func valueString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
