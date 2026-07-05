package streamproc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/redaction"
)

var (
	ErrAlreadyRunning = errors.New("stream process is already running")
	ErrNotRunning     = errors.New("stream process is not running")
)

type Starter interface {
	Start(ctx context.Context, bin string, args []string) (RunningProcess, error)
}

type RunningProcess interface {
	PID() int
	Wait() error
	Terminate() error
	Kill() error
}

type ExecStarter struct{}

func (ExecStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, errors.New("ffmpeg binary is required")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execProcess{cmd: cmd, stdin: stdin}, nil
}

type execProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func (p execProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p execProcess) Terminate() error {
	if p.stdin == nil {
		return nil
	}
	_, err := io.WriteString(p.stdin, "q\n")
	closeErr := p.stdin.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (p execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

type Manager struct {
	ArchiveRoot              string
	FFmpegBin                string
	Profile                  ffmpeg.EncoderProfile
	Starter                  Starter
	Reporter                 Reporter
	ArtifactReporter         ArtifactReporter
	Packager                 ArchivePackager
	MetricsInterval          time.Duration
	PackageTimeout           time.Duration
	ArtifactReportTimeout    time.Duration
	InputAllowedHosts        []string
	InputResolver            ffmpeg.HostResolver
	AllowDirectHLS           bool
	AllowHostnameInputs      bool
	RequireInputAllowedHosts bool
	OutputRelayURL           string
	RequireOutputRelay       bool

	mu        sync.Mutex
	processes map[string]*trackedProcess
}

type Reporter interface {
	Report(ctx context.Context, signal observability.Signal) error
}

type ArtifactReporter interface {
	ReportArtifacts(ctx context.Context, streamID string, artifacts []control.Artifact) error
}

type ArchivePackager interface {
	Package(ctx context.Context, job lifecycle.PackageJob) (lifecycle.Result, error)
}

type trackedProcess struct {
	snapshot Snapshot
	process  RunningProcess
	job      lifecycle.StreamJob
	done     chan error
}

type Snapshot struct {
	StreamID     string            `json:"stream_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	PID          int               `json:"pid,omitempty"`
	StartedAtJST string            `json:"started_at_jst"`
	StoppedAtJST string            `json:"stopped_at_jst,omitempty"`
	Archive      map[string]string `json:"archive"`
	Error        string            `json:"error,omitempty"`
}

func NewManagerFromEnv() *Manager {
	obs := observability.NewClientFromEnv()
	var reporter Reporter
	if obs.Enabled() {
		reporter = obs
	}
	archiveRoot := envDefault("AUTOSTREAM_ARCHIVE_DIR", "/var/lib/autostream/archives")
	ffmpegBin := envDefault("FFMPEG_BIN", "ffmpeg")
	controlConfig := control.ConfigFromEnv()
	var artifactReporter ArtifactReporter
	if controlConfig.ControlPanelURL != "" && controlConfig.Token != "" {
		artifactReporter = control.Client{Config: controlConfig}
		if reporter == nil {
			reporter = control.Client{Config: controlConfig}
		}
	}
	return &Manager{
		ArchiveRoot:              archiveRoot,
		FFmpegBin:                ffmpegBin,
		Starter:                  ExecStarter{},
		Reporter:                 reporter,
		ArtifactReporter:         artifactReporter,
		MetricsInterval:          envDuration("ENCODER_METRICS_INTERVAL_SEC", 10*time.Second),
		PackageTimeout:           envDuration("ENCODER_PACKAGE_TIMEOUT_SEC", 2*time.Hour),
		ArtifactReportTimeout:    envDuration("ENCODER_ARTIFACT_REPORT_TIMEOUT_SEC", 10*time.Second),
		InputAllowedHosts:        splitCSV(envDefault("AUTOSTREAM_INPUT_ALLOWED_HOSTS", os.Getenv("ENCODER_INPUT_ALLOWED_HOSTS"))),
		AllowDirectHLS:           envBool("AUTOSTREAM_ALLOW_DIRECT_HLS_INPUT", false),
		AllowHostnameInputs:      envBool("AUTOSTREAM_ALLOW_HOSTNAME_INPUTS", false),
		RequireInputAllowedHosts: envBool("AUTOSTREAM_REQUIRE_INPUT_ALLOWED_HOSTS", true),
		OutputRelayURL:           strings.TrimSpace(os.Getenv("AUTOSTREAM_OUTPUT_RELAY_URL")),
		RequireOutputRelay:       envBool("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOSTREAM_ENV")), "production")),
		Packager: lifecycle.Manager{
			ArchiveRoot: archiveRoot,
			FFmpegBin:   ffmpegBin,
			Runner:      ffmpeg.CommandRunner{},
			Uploader: archive.RetryUploader{
				Inner:  uploaderFromEnv(false),
				Policy: archive.RetryPolicy{MaxAttempts: envInt("GOOGLE_DRIVE_UPLOAD_RETRY_MAX", 5), BaseDelay: time.Duration(envInt("GOOGLE_DRIVE_UPLOAD_RETRY_BASE_DELAY_SEC", 2)) * time.Second},
			},
		},
	}
}

func (m *Manager) Start(job lifecycle.StreamJob) (Snapshot, error) {
	if job.StreamID == "" || job.Name == "" {
		return Snapshot{}, errors.New("stream id and name are required")
	}
	if job.InputURL == "" {
		return Snapshot{}, errors.New("input_url is required")
	}
	validateCtx, cancelValidate := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelValidate()
	if err := ffmpeg.ValidateInputTargetWithRuntimePolicy(validateCtx, job.InputURL, m.InputAllowedHosts, m.InputResolver, ffmpeg.RuntimeInputPolicy{AllowDirectHLS: m.AllowDirectHLS, AllowHostnameInputs: m.AllowHostnameInputs, RequireAllowedHosts: m.RequireInputAllowedHosts}); err != nil {
		return Snapshot{}, err
	}
	if job.RTMPURL == "" {
		return Snapshot{}, errors.New("rtmp_url is required")
	}
	if job.StreamKey == "" {
		return Snapshot{}, errors.New("stream key is required")
	}
	if err := ffmpeg.ValidateOutputTarget(job.RTMPURL, job.StreamKey); err != nil {
		return Snapshot{}, err
	}
	outputTarget, err := m.liveOutputTarget(job)
	if err != nil {
		return Snapshot{}, err
	}
	layout, err := archive.NewLayout(m.archiveRoot(), job.StreamID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := m.validateInputForLayout(job, layout); err != nil {
		return Snapshot{}, err
	}
	if err := ensureLiveArchiveDir(layout.RootDir, filepath.Join(layout.RootDir, "tmp"), layout.TmpDir()); err != nil {
		return Snapshot{}, err
	}
	if err := archive.ReserveOutputFileNoSymlink(layout.RootDir, layout.FinalMKV()); err != nil {
		return Snapshot{}, err
	}
	for _, output := range []string{layout.TmpFFmpegProgress(), layout.TmpFFmpegAudioStats()} {
		if err := rejectExistingArchiveOutputSymlink(output); err != nil {
			return Snapshot{}, err
		}
	}
	profile := m.Profile
	if profile.Width == 0 {
		profile = ffmpeg.DefaultProfile()
	}
	args := lifecycle.BuildLiveArgsToOutputTarget(job, outputTarget, layout.FinalMKV(), layout.TmpFFmpegProgress(), layout.TmpFFmpegAudioStats(), profile)
	starter := m.Starter
	if starter == nil {
		starter = ExecStarter{}
	}
	startedAt := job.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	job.StartedAt = startedAt

	m.mu.Lock()
	if m.processes == nil {
		m.processes = map[string]*trackedProcess{}
	}
	if existing, ok := m.processes[job.StreamID]; ok && (existing.snapshot.Status == "starting" || existing.snapshot.Status == "running") {
		m.mu.Unlock()
		return Snapshot{}, ErrAlreadyRunning
	}
	m.processes[job.StreamID] = &trackedProcess{snapshot: Snapshot{StreamID: job.StreamID, Name: job.Name, Status: "starting", StartedAtJST: startedAt.In(jst()).Format(time.RFC3339)}, job: job}
	m.mu.Unlock()

	process, err := starter.Start(context.Background(), m.ffmpegBin(), args)
	if err != nil {
		m.mu.Lock()
		delete(m.processes, job.StreamID)
		m.mu.Unlock()
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		StreamID:     job.StreamID,
		Name:         job.Name,
		Status:       "running",
		PID:          process.PID(),
		Archive:      lifecycle.ArchiveArtifacts(job.StreamID),
		StartedAtJST: startedAt.In(jst()).Format(time.RFC3339),
	}
	if err := writeStartMetadata(layout, job, snapshot, args, m.ffmpegBin()); err != nil {
		_ = process.Kill()
		m.mu.Lock()
		delete(m.processes, job.StreamID)
		m.mu.Unlock()
		return Snapshot{}, err
	}

	done := make(chan error, 1)
	m.mu.Lock()
	m.processes[job.StreamID] = &trackedProcess{snapshot: snapshot, process: process, job: job, done: done}
	m.mu.Unlock()

	m.report(observability.Signal{
		Type:      "event",
		Name:      "encoder.process.started",
		StreamID:  job.StreamID,
		Status:    "running",
		Timestamp: time.Now().UTC(),
		Attributes: map[string]any{
			"recording_mkv": "final.mkv",
		},
	})
	go m.wait(job.StreamID, process, done)
	go m.monitor(job.StreamID, layout.FinalMKV(), layout.TmpFFmpegProgress(), layout.TmpFFmpegAudioStats())
	return snapshot, nil
}

func (m *Manager) validateInputForLayout(job lifecycle.StreamJob, layout archive.Layout) error {
	if job.InputMode == "discord_opus_rtp" {
		if !strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_discord_audio:") {
			return ffmpeg.ErrUnsafeInputTarget
		}
		got := filepath.Clean(ffmpeg.ResolveInputTarget(job.InputURL))
		want := filepath.Clean(layout.TmpDiscordOpusSDP())
		if got != want {
			return ffmpeg.ErrUnsafeInputTarget
		}
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_discord_audio:") {
		return ffmpeg.ErrUnsafeInputTarget
	}
	return nil
}

func (m *Manager) liveOutputTarget(job lifecycle.StreamJob) (string, error) {
	if strings.TrimSpace(m.OutputRelayURL) == "" {
		if m.RequireOutputRelay {
			return "", errors.New("output relay URL is required")
		}
		return job.RTMPURL + "/" + job.StreamKey, nil
	}
	target := relayOutputTarget(m.OutputRelayURL, job.StreamID)
	if err := ffmpeg.ValidateRelayOutputTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

func relayOutputTarget(template, streamID string) string {
	template = strings.TrimSpace(template)
	escapedStreamID := url.PathEscape(strings.TrimSpace(streamID))
	if strings.Contains(template, "{stream_id}") {
		return strings.ReplaceAll(template, "{stream_id}", escapedStreamID)
	}
	return strings.TrimRight(template, "/") + "/" + escapedStreamID
}

func (m *Manager) Stop(streamID string) (Snapshot, error) {
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok || tracked.snapshot.Status != "running" {
		m.mu.Unlock()
		return Snapshot{}, ErrNotRunning
	}
	tracked.snapshot.Status = "stopping"
	snapshot := tracked.snapshot
	m.mu.Unlock()

	m.report(observability.Signal{
		Type:      "event",
		Name:      "encoder.process.stopping",
		StreamID:  streamID,
		Status:    "stopping",
		Timestamp: time.Now().UTC(),
	})
	if err := m.stopProcessGracefully(tracked.process, tracked.done); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (m *Manager) StopAll() []error {
	m.mu.Lock()
	streamIDs := make([]string, 0, len(m.processes))
	for streamID, tracked := range m.processes {
		if tracked.snapshot.Status == "running" {
			streamIDs = append(streamIDs, streamID)
		}
	}
	m.mu.Unlock()
	errs := make([]error, 0)
	for _, streamID := range streamIDs {
		if _, err := m.Stop(streamID); err != nil && !errors.Is(err, ErrNotRunning) {
			errs = append(errs, err)
		}
	}
	return errs
}

func (m *Manager) StopAllAndDrain(ctx context.Context) []error {
	errs := m.StopAll()
	if err := m.Drain(ctx); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func (m *Manager) Drain(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if m.isDrained() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) isDrained() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tracked := range m.processes {
		switch tracked.snapshot.Status {
		case "starting", "running", "stopping", "packaging":
			return false
		}
	}
	return true
}

func (m *Manager) stopProcessGracefully(process RunningProcess, done <-chan error) error {
	if err := process.Terminate(); err != nil {
		if killErr := process.Kill(); killErr != nil {
			return killErr
		}
		return err
	}
	select {
	case <-done:
		return nil
	case <-time.After(stopGracePeriod()):
		return process.Kill()
	}
}

func stopGracePeriod() time.Duration {
	raw := strings.TrimSpace(os.Getenv("FFMPEG_STOP_GRACE_SEC"))
	if raw == "" {
		return 5 * time.Second
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 {
		return 5 * time.Second
	}
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func (m *Manager) Status(streamID string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tracked, ok := m.processes[streamID]
	if !ok {
		return Snapshot{}, ErrNotRunning
	}
	return tracked.snapshot, nil
}

func (m *Manager) CurrentStreamID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for streamID, tracked := range m.processes {
		if tracked.snapshot.Status == "running" || tracked.snapshot.Status == "stopping" || tracked.snapshot.Status == "packaging" {
			return streamID
		}
	}
	return ""
}

func (m *Manager) HeartbeatMetrics() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	metrics := map[string]float64{
		"encoder.process_alive":        0,
		"encoder.active_process_count": 0,
	}
	for _, tracked := range m.processes {
		if tracked.snapshot.Status == "running" || tracked.snapshot.Status == "stopping" {
			metrics["encoder.process_alive"] = 1
			metrics["encoder.active_process_count"]++
		}
	}
	return metrics
}

func (m *Manager) wait(streamID string, process RunningProcess, done chan<- error) {
	err := process.Wait()
	if done != nil {
		done <- err
	}
	var signal observability.Signal
	var shouldPackage bool
	var packageJob lifecycle.PackageJob
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok {
		m.mu.Unlock()
		return
	}
	tracked.snapshot.StoppedAtJST = time.Now().In(jst()).Format(time.RFC3339)
	if err != nil && tracked.snapshot.Status != "stopping" {
		redactedError := redaction.Message(err.Error(), tracked.job.StreamKey, tracked.job.InputURL, tracked.job.RTMPURL, tracked.job.RTMPURL+"/"+tracked.job.StreamKey)
		tracked.snapshot.Status = "failed"
		tracked.snapshot.Error = redactedError
		signal = observability.Signal{
			Type:      "error",
			Name:      "encoder.process.exited",
			StreamID:  streamID,
			Status:    "failed",
			Timestamp: time.Now().UTC(),
			Attributes: map[string]any{
				"error": redactedError,
			},
		}
	} else {
		shouldPackage = m.Packager != nil
		if shouldPackage {
			tracked.snapshot.Status = "packaging"
		} else {
			tracked.snapshot.Status = "stopped"
		}
		packageJob = lifecycle.PackageJob{StreamID: tracked.job.StreamID, Name: tracked.job.Name, StartedAt: tracked.job.StartedAt, ArchiveConfig: tracked.job.ArchiveConfig}
		signal = observability.Signal{
			Type:      "event",
			Name:      "encoder.process.stopped",
			StreamID:  streamID,
			Status:    "stopped",
			Timestamp: time.Now().UTC(),
		}
	}
	scrubTrackedProcessJob(tracked)
	m.mu.Unlock()
	m.report(signal)
	m.reportMetric(streamID, "encoder.process_alive", 0)
	if shouldPackage {
		m.packageArchive(packageJob)
	}
}

func (m *Manager) monitor(streamID, finalMKV, progressPath, audioStatsPath string) {
	interval := m.MetricsInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	silenceThresholdDB := envFloat("ENCODER_AUDIO_SILENCE_THRESHOLD_DB", -50)
	clippingThresholdDB := envFloat("ENCODER_AUDIO_CLIPPING_THRESHOLD_DB", -1)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var previousSize int64 = -1
	var previousAt time.Time
	lastProgressAt := time.Now().UTC()
	var lastProgressMod time.Time
	var silenceSec float64
	var clippingTotal float64
	for {
		if !m.isRunning(streamID) {
			return
		}
		now := time.Now().UTC()
		elapsedForAudio := interval.Seconds()
		if !previousAt.IsZero() {
			elapsedForAudio = now.Sub(previousAt).Seconds()
		}
		size := fileSize(finalMKV)
		m.reportMetric(streamID, "encoder.process_alive", 1)
		m.reportMetric(streamID, "recorder.file_size_bytes", float64(size))
		if previousSize >= 0 && !previousAt.IsZero() {
			elapsed := now.Sub(previousAt).Seconds()
			if elapsed > 0 {
				kbps := float64(size-previousSize) * 8 / 1000 / elapsed
				if kbps < 0 {
					kbps = 0
				}
				m.reportMetric(streamID, "recorder.write_bitrate_kbps", kbps)
			}
		}
		if free, ok := diskFreeBytes(finalMKV); ok {
			m.reportMetric(streamID, "recorder.disk_free_bytes", float64(free))
		}
		m.reportFFmpegProgress(streamID, progressPath)
		m.reportAudioStats(streamID, audioStatsPath, elapsedForAudio, silenceThresholdDB, clippingThresholdDB, &silenceSec, &clippingTotal)
		if modTime, ok := fileModTime(progressPath); ok && modTime.After(lastProgressMod) {
			lastProgressMod = modTime
			lastProgressAt = now
		}
		m.reportMetric(streamID, "media.input_timeout_sec", maxFloat(now.Sub(lastProgressAt).Seconds(), 0))
		previousSize = size
		previousAt = now
		<-ticker.C
	}
}

func (m *Manager) isRunning(streamID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	tracked, ok := m.processes[streamID]
	return ok && tracked.snapshot.Status == "running"
}

func (m *Manager) packageArchive(job lifecycle.PackageJob) {
	started := time.Now().UTC()
	m.mu.Lock()
	tracked, ok := m.processes[job.StreamID]
	if ok {
		tracked.snapshot.Status = "packaging"
	}
	m.mu.Unlock()
	m.report(observability.Signal{
		Type:      "event",
		Name:      "archive.package.started",
		StreamID:  job.StreamID,
		Status:    "packaging",
		Timestamp: time.Now().UTC(),
	})
	packageCtx, cancelPackage := context.WithTimeout(context.Background(), m.packageTimeout())
	result, err := m.Packager.Package(packageCtx, job)
	cancelPackage()
	elapsed := time.Since(started)
	if err != nil {
		m.reportArchiveFileMetrics(job.StreamID)
		phase := lifecycle.ErrorPhase(err)
		if phase == "upload" {
			m.reportMetric(job.StreamID, "archive.package_status", 1)
			m.reportMetric(job.StreamID, "gdrive.upload_status", 0)
		} else {
			m.reportMetric(job.StreamID, "archive.package_status", 0)
		}
		m.reportMetric(job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
		m.report(observability.Signal{
			Type:       "error",
			Name:       "archive.package.failed",
			StreamID:   job.StreamID,
			Status:     "failed",
			Timestamp:  time.Now().UTC(),
			Attributes: packageFailureAttributes(err),
		})
		m.mu.Lock()
		tracked, ok = m.processes[job.StreamID]
		if ok {
			tracked.snapshot.Status = "package_failed"
			tracked.snapshot.Error = lifecycle.SafeErrorSummary(err)
			scrubTrackedProcessJob(tracked)
		}
		m.mu.Unlock()
		return
	}
	m.reportArchiveFileMetrics(job.StreamID)
	m.reportMetric(job.StreamID, "archive.package_status", 1)
	m.reportMetric(job.StreamID, "archive.final_mp4_exists", 1)
	m.reportMetric(job.StreamID, "recorder.remux_duration_ms", result.RemuxDurationMS)
	m.reportMetric(job.StreamID, "gdrive.upload_status", 1)
	m.reportMetric(job.StreamID, "gdrive.upload_retry_count", float64(maxInt(result.Metadata.Upload.Attempts-1, 0)))
	m.reportMetric(job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
	m.reportMetric(job.StreamID, "gdrive.upload_file_count", float64(result.Metadata.Upload.UploadedFileCount()))
	m.reportMetric(job.StreamID, "gdrive.upload_folder_fingerprint_present", boolMetric(result.Metadata.Upload.HasFolderFingerprint()))
	m.reportMetric(job.StreamID, "gdrive.upload_final_mp4_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("final.mp4")))
	m.reportMetric(job.StreamID, "gdrive.upload_metadata_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("metadata.json")))
	m.report(observability.Signal{
		Type:      "event",
		Name:      "archive.package.completed",
		StreamID:  job.StreamID,
		Status:    "completed",
		Timestamp: time.Now().UTC(),
		Attributes: map[string]any{
			"upload_dry_run":    result.Metadata.Upload.DryRun,
			"upload_attempts":   result.Metadata.Upload.Attempts,
			"file_count":        len(result.Metadata.Upload.FileIDs),
			"remux_duration_ms": result.RemuxDurationMS,
		},
	})
	if m.ArtifactReporter != nil {
		artifacts := control.ArchiveArtifacts(result.Layout)
		if len(artifacts) > 0 {
			reportCtx, cancelReport := context.WithTimeout(context.Background(), m.artifactReportTimeout())
			err := m.ArtifactReporter.ReportArtifacts(reportCtx, job.StreamID, artifacts)
			cancelReport()
			if err != nil {
				m.report(observability.Signal{
					Type:      "warning",
					Name:      "archive.artifact_report.failed",
					StreamID:  job.StreamID,
					Status:    "warning",
					Timestamp: time.Now().UTC(),
					Attributes: map[string]any{
						"artifact_count": len(artifacts),
						"error_class":    "control_panel_artifact_report_failed",
					},
				})
			} else {
				m.report(observability.Signal{
					Type:       "event",
					Name:       "archive.artifact_report.completed",
					StreamID:   job.StreamID,
					Status:     "completed",
					Timestamp:  time.Now().UTC(),
					Attributes: map[string]any{"artifact_count": len(artifacts)},
				})
			}
		}
	}
	m.mu.Lock()
	tracked, ok = m.processes[job.StreamID]
	if ok {
		tracked.snapshot.Status = "completed"
		tracked.snapshot.Archive["final_artifact_set"] = "final/" + job.StreamID
		tracked.snapshot.Archive["final_mp4"] = "final.mp4"
		scrubTrackedProcessJob(tracked)
	}
	m.mu.Unlock()
}

func scrubTrackedProcessJob(tracked *trackedProcess) {
	if tracked == nil {
		return
	}
	job := tracked.job
	job.InputURL = ""
	job.RTMPURL = ""
	job.StreamKey = ""
	job.ArchiveConfig.FolderID = ""
	job.ArchiveConfig.ServiceAccountJSON = ""
	job.ArchiveConfig.ClientSecret = ""
	job.ArchiveConfig.RefreshToken = ""
	tracked.job = job
}

func (m *Manager) packageTimeout() time.Duration {
	if m.PackageTimeout > 0 {
		return m.PackageTimeout
	}
	return 2 * time.Hour
}

func (m *Manager) artifactReportTimeout() time.Duration {
	if m.ArtifactReportTimeout > 0 {
		return m.ArtifactReportTimeout
	}
	return 10 * time.Second
}

func (m *Manager) reportArchiveFileMetrics(streamID string) {
	layout, err := archive.NewLayout(m.archiveRoot(), streamID)
	if err != nil {
		return
	}
	if fileSize(layout.FinalMKV()) > 0 {
		m.reportMetric(streamID, "archive.final_mkv_exists", 1)
	} else {
		m.reportMetric(streamID, "archive.final_mkv_exists", 0)
	}
	if fileSize(layout.FinalMP4()) > 0 {
		m.reportMetric(streamID, "archive.final_mp4_exists", 1)
	} else {
		m.reportMetric(streamID, "archive.final_mp4_exists", 0)
	}
}

func packageFailureAttributes(err error) map[string]any {
	phase := lifecycle.ErrorPhase(err)
	if phase == "" {
		phase = "unknown"
	}
	return map[string]any{
		"failure_phase": phase,
		"error_class":   lifecycle.ErrorClass(err),
	}
}

func (m *Manager) report(signal observability.Signal) {
	if m.Reporter == nil {
		return
	}
	_ = m.Reporter.Report(context.Background(), signal)
}

func (m *Manager) reportMetric(streamID, name string, value float64) {
	m.report(observability.Signal{
		Type:      "metric",
		Name:      name,
		StreamID:  streamID,
		Value:     &value,
		Timestamp: time.Now().UTC(),
	})
}

func (m *Manager) reportFFmpegProgress(streamID, progressPath string) {
	body, err := os.ReadFile(progressPath)
	if err != nil {
		return
	}
	progress := ffmpeg.ParseProgress(string(body))
	if progress.FPS > 0 {
		m.reportMetric(streamID, "encoder.output_fps", progress.FPS)
	}
	if progress.BitrateKbps > 0 {
		m.reportMetric(streamID, "encoder.output_bitrate_kbps", progress.BitrateKbps)
	}
	m.reportMetric(streamID, "encoder.dropped_frames_total", progress.DroppedFrames)
}

func (m *Manager) reportAudioStats(streamID, audioStatsPath string, elapsedSec, silenceThresholdDB, clippingThresholdDB float64, silenceSec, clippingTotal *float64) {
	body, err := os.ReadFile(audioStatsPath)
	if err != nil {
		return
	}
	stats := ffmpeg.ParseAudioStats(string(body))
	if stats.HasRMS {
		m.reportMetric(streamID, "encoder.audio_level_db", stats.RMSLevelDB)
		if stats.RMSLevelDB <= silenceThresholdDB {
			*silenceSec += maxFloat(elapsedSec, 0)
		} else {
			*silenceSec = 0
		}
		m.reportMetric(streamID, "encoder.audio_silence_sec", *silenceSec)
	}
	if stats.HasPeak {
		if stats.PeakLevelDB >= clippingThresholdDB {
			*clippingTotal++
		}
		m.reportMetric(streamID, "encoder.audio_clipping_total", *clippingTotal)
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func fileModTime(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func boolMetric(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func writeStartMetadata(layout archive.Layout, job lifecycle.StreamJob, snapshot Snapshot, args []string, bin string) error {
	extra := map[string]any{"live_process": true}
	if job.InputMode != "" {
		extra["input_mode"] = job.InputMode
	}
	metadata := map[string]any{
		"stream_id":      job.StreamID,
		"name":           job.Name,
		"started_at_jst": snapshot.StartedAtJST,
		"archive":        snapshot.Archive,
		"commands": lifecycle.RedactCommandsForLayout(layout, []ffmpeg.Command{{
			Bin:  bin,
			Args: args,
		}}, job.StreamKey, job.InputURL, job.RTMPURL),
		"extra": extra,
	}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := lifecycle.WriteFileNoSymlink(layout.TmpMetadata(), append(body, '\n'), 0o640); err != nil {
		return err
	}
	logLine, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "event": "stream_process.started", "stream_id": job.StreamID})
	return lifecycle.WriteFileNoSymlink(layout.TmpLogs(), append(logLine, '\n'), 0o640)
}

func ensureLiveArchiveDir(rootDir string, dirs ...string) error {
	for _, dir := range append([]string{rootDir}, dirs...) {
		if err := ensureSingleLiveArchiveDir(rootDir, dir); err != nil {
			return err
		}
	}
	return nil
}

func ensureSingleLiveArchiveDir(rootDir, dir string) error {
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return err
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, dirAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("archive directory must stay under archive root")
	}
	if err := os.MkdirAll(dirAbs, 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(dirAbs)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive directory must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("archive path component must be a directory")
	}
	return nil
}

func rejectExistingArchiveOutputSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive path must not be a symlink")
	}
	return nil
}

func (m *Manager) archiveRoot() string {
	return valueOrDefault(m.ArchiveRoot, "/var/lib/autostream/archives")
}

func (m *Manager) ffmpegBin() string {
	return valueOrDefault(m.FFmpegBin, "ffmpeg")
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func envDefault(key, fallback string) string {
	return valueOrDefault(os.Getenv(key), fallback)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	var out int
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return fallback
		}
		out = out*10 + int(ch-'0')
	}
	if out <= 0 {
		return fallback
	}
	return out
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value + "s")
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func uploaderFromEnv(dryRun bool) archive.ArchiveUploader {
	if dryRun || os.Getenv("GOOGLE_DRIVE_AUTH_MODE") == "" {
		return archive.DryRunUploader{}
	}
	return archive.GoogleDriveAPIUploader{Config: archive.GoogleDriveConfigFromEnv()}
}

func jst() *time.Location {
	return time.FixedZone("JST", 9*60*60)
}
