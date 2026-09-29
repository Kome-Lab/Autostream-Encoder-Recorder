package streamproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

// Inject a primary-open failure after production safely reserves its output.
// No argv, witness, process status, or codec behavior is substituted.
type archiveFailureStarter struct {
	path    string
	process RunningProcess
}

func (s *archiveFailureStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if err := os.Remove(s.path); err != nil {
		return nil, err
	}
	if err := os.Mkdir(s.path, 0750); err != nil {
		return nil, err
	}
	p, err := (ExecStarter{}).Start(ctx, bin, args)
	s.process = p
	return p, err
}

func TestArchiveIntegrityFailureNeverCommitsRunning(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	m, _, job, id := preparationManager(t, &preparationWitness{})
	m.Profile = ffmpeg.EncoderProfile{Width: 160, Height: 90, FPS: 10, VideoBitrate: "300k", AudioBitrate: "64k", SampleRate: 48000, KeyframeSec: 2}
	m.CoverWitness = nil    // use the production feed/output witness, not the fixture.
	m.CoverApplyTimeout = 0 // use the unchanged production six-second limit.
	audio := audioingest.NewManager(m.ArchiveRoot)
	bridge, err := audio.StartBridge(job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	defer audio.StopOwnedBridge(bridge)
	job.InputURL = bridge.InputURL
	job.InputMode = "discord_opus_rtp"
	layout, err := archive.NewLayout(m.ArchiveRoot, job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(layout.FinalMKV(), filepath.Clean(m.ArchiveRoot)+string(os.PathSeparator)) {
		t.Fatal("unsafe test target")
	}
	starter := &archiveFailureStarter{path: layout.FinalMKV()}
	m.Starter = starter
	p, ctx, err := m.BeginStartPreparation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPreparationCleanup(p, func() { audio.StopOwnedBridge(bridge) }); err != nil {
		t.Fatal(err)
	}
	_, err = m.PrepareStart(ctx, p, job)
	m.FinishStartPreparation(p)
	if err != nil {
		t.Fatalf("prepare must own actual waiting FFmpeg before failure: %v", err)
	}
	defer m.AbortStartPreparation(context.Background(), id)
	state, err := m.CommitStartPreparation(context.Background(), id)
	if err == nil || state.Phase == "running" {
		t.Fatalf("mandatory primary failure became successful commit: phase=%q err=%v", state.Phase, err)
	}
	select {
	case <-p.exited:
	case <-time.After(8 * time.Second):
		t.Fatal("owned FFmpeg was not reaped")
	}
	select {
	case <-p.cleanupDone:
	case <-time.After(3 * time.Second):
		t.Fatal("owned preparation resources not cleaned")
	}
	status, _ := m.Status(id.StreamID)
	if status.Status == "running" || status.Status == "packaging" || status.Status == "stopped" {
		t.Fatalf("false successful terminal status: %+v", status)
	}
	final, e := m.StartPreparationStatus(id.StreamID, id.StartID)
	if e == nil && (final.Phase == "running" || final.CoverState != nil) {
		t.Fatalf("fatal preparation exposed applied cover: %+v", final)
	}
	stderr := processStderr(starter.process)
	if !strings.Contains(stderr, "error opening") || !strings.Contains(stderr, "Slave") {
		t.Fatalf("failure was not mandatory tee open: %s", stderr)
	}
	t.Logf("actual FFmpeg PID=%d; commit_error=%v; final_status=%s; phase=%s; original stderr:\n%s", starter.process.PID(), err, status.Status, final.Phase, stderr)
}
