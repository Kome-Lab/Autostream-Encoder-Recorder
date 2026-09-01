package streamproc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/redaction"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
	"github.com/example/autostream-encoder-recorder/internal/watermarkfeed"
)

var (
	ErrAlreadyRunning                    = errors.New("stream process is already running")
	ErrLiveAPIRequiresManagedOutputRelay = outputrelay.ErrLiveAPIRequiresManagedOutputRelay
	ErrLiveAPIRelayBindingMismatch       = outputrelay.ErrLiveAPIRelayBindingMismatch
	ErrLiveAPIRelayStaticNotReady        = outputrelay.ErrLiveAPIRelayStaticNotReady
	// ErrAlreadyStopped identifies a known stream whose media process has
	// already entered a non-running terminal or stop-in-progress state.  It is
	// deliberately distinct from ErrNotRunning: callers can safely make a
	// target-specific retry a no-op without treating an unknown stream ID as a
	// successful stop.
	ErrAlreadyStopped = errors.New("stream process is already stopped")
	// ErrStarting identifies the narrow window after a stream reservation was
	// accepted but before FFmpeg became a running process. It must not be
	// normalized as an idle/no-process receipt by upstream callers.
	ErrStarting               = errors.New("stream process is starting")
	ErrNotRunning             = errors.New("stream process is not running")
	ErrInvalidRuntimeSettings = errors.New("invalid encoder runtime settings")
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

const maxFFmpegStderrBytes = 8 << 10

// boundedStderr keeps only the tail of FFmpeg stderr.  FFmpeg reports the
// useful failure near the end of its diagnostic output, while an unbounded
// pipe would allow a failed process to grow the Encoder's memory usage.
type boundedStderr struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= maxFFmpegStderrBytes {
		b.data = append(b.data[:0], p[len(p)-maxFFmpegStderrBytes:]...)
		b.truncated = true
		return len(p), nil
	}
	b.data = append(b.data, p...)
	if len(b.data) > maxFFmpegStderrBytes {
		drop := len(b.data) - maxFFmpegStderrBytes
		b.data = append([]byte(nil), b.data[drop:]...)
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedStderr) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.data))
	if text == "" {
		return ""
	}
	if b.truncated {
		return "[truncated] " + text
	}
	return text
}

func (ExecStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, errors.New("ffmpeg binary is required")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	stderr := &boundedStderr{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stderr: stderr}, nil
}

type execProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *boundedStderr
	stdinMu sync.Mutex
}

type runtimeCommander interface {
	Command(target, command, argument string) error
}

func (p *execProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Stderr() string {
	if p == nil {
		return ""
	}
	return p.stderr.String()
}

func (p *execProcess) Command(target, command, argument string) error {
	if target != "volume@gain" || command != "volume" {
		return errors.New("unsupported ffmpeg runtime command")
	}
	value, err := strconv.ParseFloat(strings.TrimSuffix(argument, "dB"), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < -60 || value > 24 {
		return ErrInvalidRuntimeSettings
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdin == nil {
		return errors.New("ffmpeg runtime command input is unavailable")
	}
	// FFmpeg consumes the leading "c" as a single interactive key, then reads
	// the command from the remainder of that same input line. A newline directly
	// after "c" is therefore parsed as an empty command.
	// The command itself expects: target, time, command, argument.
	// A time of -1 applies the command immediately to the first matching
	// named filter instance.
	_, err = io.WriteString(p.stdin, "c"+target+" -1 "+command+" "+argument+"\n")
	return err
}

func (p *execProcess) Terminate() error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
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

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

type Manager struct {
	ArchiveRoot              string
	StopReceiptTTL           time.Duration
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
	OutputRelayMode          string
	OutputRelayBindingID     string
	RequireOutputRelay       bool
	ProcessExitHook          func(streamID string)
	CoverAssets              *videocover.Loader
	CoverWitness             CoverGraphWitness
	WatermarkWitness         WatermarkGraphWitness
	CoverApplyTimeout        time.Duration
	CoverFetchTimeout        time.Duration

	mu               sync.Mutex
	processes        map[string]*trackedProcess
	coverGenerations map[string]coverGeneration
}

// CoverGraphWitness is the narrow graph-observation seam. Production waits
// for the exact feed version to be consumed and then for output progress to
// advance. Tests can provide a deterministic witness without launching
// FFmpeg.
type CoverGraphWitness interface {
	Apply(ctx context.Context, source *imagefeed.Source, frame []byte, initial bool, progressPath string) error
}

// WatermarkGraphWitness mirrors the Cover graph witness for the independently
// controlled Watermark feed. A successful source update alone is not enough:
// production also waits for downstream output progress before the new layer
// revision can appear in applied_witness.watermark.
type WatermarkGraphWitness interface {
	Apply(ctx context.Context, source *watermarkfeed.Source, frame []byte, progressPath string) error
}

type coverGeneration struct {
	JobGeneration uint64
	Generation    uint64
}

type Reporter interface {
	Report(ctx context.Context, signal observability.Signal) error
}

type ArtifactReporter interface {
	ReportArtifacts(ctx context.Context, streamID string, artifacts []control.Artifact, archiveRuns ...control.ArchiveRun) error
}

type ArchivePackager interface {
	Package(ctx context.Context, job lifecycle.PackageJob) (lifecycle.Result, error)
}

type trackedProcess struct {
	snapshot           Snapshot
	process            RunningProcess
	job                lifecycle.StreamJob
	done               chan error
	watermark          *watermarkfeed.Source
	runtimeMu          sync.Mutex
	watermarkMu        sync.Mutex
	watermarkState     videocover.LayerState
	cover              *imagefeed.Source
	coverMu            sync.Mutex
	coverState         videocover.RuntimeState
	terminalCoverState *videocover.RuntimeState
	coverReplay        map[string]coverReplay
	coverReplayOrder   []string
	transparentCover   []byte
	progressPath       string
}

type coverReplay struct {
	fingerprint [32]byte
	response    videocover.ApplyResponse
}

const maxCoverReplayEntries = 256

type Snapshot struct {
	StreamID           string            `json:"stream_id"`
	Name               string            `json:"name"`
	Status             string            `json:"status"`
	PID                int               `json:"pid,omitempty"`
	StartedAtJST       string            `json:"started_at_jst"`
	StoppedAtJST       string            `json:"stopped_at_jst,omitempty"`
	Archive            map[string]string `json:"archive"`
	Error              string            `json:"error,omitempty"`
	EncoderAudioGainDB float64           `json:"encoder_audio_gain_db"`
	OverlayProfileID   string            `json:"overlay_profile_id,omitempty"`
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
	var coverAssets *videocover.Loader
	if controlConfig.ControlPanelURL != "" && controlConfig.Token != "" {
		controlClient := control.Client{Config: controlConfig}
		artifactReporter = controlClient
		if controlConfig.Validate() == nil {
			coverAssets = videocover.NewLoader(controlClient, envInt("VIDEO_COVER_CACHE_ENTRIES", 16), int64(envInt("VIDEO_COVER_CACHE_MIB", 64))<<20)
		}
		if reporter == nil {
			reporter = controlClient
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
		OutputRelayMode:          strings.TrimSpace(os.Getenv("AUTOSTREAM_OUTPUT_RELAY_MODE")),
		// Binding IDs are exact canonical identities; do not trim whitespace and
		// accidentally turn an invalid persisted value into a trusted binding.
		OutputRelayBindingID: os.Getenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID"),
		RequireOutputRelay:   outputrelay.RequireRelayFromEnv(),
		CoverAssets:          coverAssets,
		CoverWitness:         progressCoverWitness{},
		WatermarkWitness:     progressWatermarkWitness{},
		CoverApplyTimeout:    envDuration("VIDEO_COVER_APPLY_TIMEOUT_SEC", 6*time.Second),
		CoverFetchTimeout:    envDuration("VIDEO_COVER_FETCH_TIMEOUT_SEC", 10*time.Second),
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
	if strings.TrimSpace(job.ArchiveRunID) != "" && job.StartedAt.IsZero() {
		return Snapshot{}, errors.New("archive run started_at is required when archive_run_id is set")
	}
	usesLocalRelay, err := m.AuthorizeOutputRelay(job)
	if err != nil {
		return Snapshot{}, err
	}
	outputRoute := "direct"
	if usesLocalRelay {
		outputRoute = "local_relay"
		// A local Relay's downstream target is configured out of band.  Do not
		// retain an upstream RTMPS target, key, or reference in process state,
		// metadata, or later error paths once the Relay route was authorized.
		clearUnusedYouTubeOutputTarget(&job)
	}
	if job.InputURL == "" {
		return Snapshot{}, errors.New("input_url is required")
	}
	validateCtx, cancelValidate := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelValidate()
	if err := ffmpeg.ValidateInputTargetWithRuntimePolicy(validateCtx, job.InputURL, m.InputAllowedHosts, m.InputResolver, ffmpeg.RuntimeInputPolicy{AllowDirectHLS: m.AllowDirectHLS, AllowHostnameInputs: m.AllowHostnameInputs, RequireAllowedHosts: m.RequireInputAllowedHosts}); err != nil {
		return Snapshot{}, err
	}
	if !usesLocalRelay {
		if job.RTMPURL == "" {
			return Snapshot{}, errors.New("rtmp_url is required")
		}
		if job.StreamKey == "" {
			return Snapshot{}, errors.New("stream key is required")
		}
		if err := ffmpeg.ValidateOutputTarget(job.RTMPURL, job.StreamKey); err != nil {
			return Snapshot{}, err
		}
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
	// A reused stream ID must never inherit an old stop receipt: a delayed
	// stop for a previous run must be able to stop this newly started target.
	if err := m.clearStopReceipt(job.StreamID); err != nil {
		return Snapshot{}, err
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
	if existing, ok := m.processes[job.StreamID]; ok {
		switch existing.snapshot.Status {
		case "starting", "running", "stopping", "packaging":
			m.mu.Unlock()
			return Snapshot{}, ErrAlreadyRunning
		}
	}
	m.processes[job.StreamID] = &trackedProcess{snapshot: Snapshot{StreamID: job.StreamID, Name: job.Name, Status: "starting", StartedAtJST: startedAt.In(jst()).Format(time.RFC3339)}, job: job}
	m.mu.Unlock()
	reservationActive := true
	defer func() {
		if !reservationActive {
			return
		}
		m.mu.Lock()
		if tracked, ok := m.processes[job.StreamID]; ok && tracked.snapshot.Status == "starting" {
			delete(m.processes, job.StreamID)
		}
		m.mu.Unlock()
	}()

	if err := ensureLiveArchiveDir(layout.RootDir, filepath.Join(layout.RootDir, "tmp"), layout.TmpDir()); err != nil {
		return Snapshot{}, err
	}
	if err := archive.PreparePreviewDir(layout); err != nil {
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
	profile := job.EncoderProfile
	if profile.Width == 0 {
		profile = m.Profile
	}
	if profile.Width == 0 {
		profile = ffmpeg.DefaultProfile()
	}
	job.EncoderProfile = profile
	transparentCover, err := videocover.TransparentFrame(profile.Width, profile.Height)
	if err != nil {
		return Snapshot{}, err
	}
	coverFrame := transparentCover
	if job.VideoCoverStart != nil {
		if err := validateCoverStart(*job.VideoCoverStart); err != nil {
			return Snapshot{}, err
		}
		if m.CoverAssets == nil {
			return Snapshot{}, videocover.NewError(videocover.ErrorCapabilityRequired)
		}
		if job.VideoCoverStart.Active {
			fetchCtx, cancelFetch := context.WithTimeout(context.Background(), m.coverFetchTimeout())
			coverFrame, err = m.CoverAssets.Load(fetchCtx, job.StreamID, *job.VideoCoverStart.CoverAsset, profile.Width, profile.Height)
			cancelFetch()
			if err != nil {
				return Snapshot{}, err
			}
		}
	}
	coverSource, err := imagefeed.New("video cover", coverFrame)
	if err != nil {
		return Snapshot{}, err
	}
	coverActive := true
	defer func() {
		if coverActive {
			_ = coverSource.Close()
		}
	}()
	watermarkSource, err := watermarkfeed.New(job.OverlayConfig)
	if err != nil {
		return Snapshot{}, err
	}
	watermarkActive := true
	defer func() {
		if watermarkActive {
			_ = watermarkSource.Close()
		}
	}()
	job.WatermarkInputURL = watermarkSource.InputURL()
	job.CoverInputURL = coverSource.InputURL()
	watermarkState := initialWatermarkState(job.OverlayProfileID, job.OverlayConfig)
	job.OverlayConfig = nil
	args := lifecycle.BuildLiveArgsToOutputTargetWithPreviewAndOverlay(job, outputTarget, layout.FinalMKV(), layout.PreviewPlaylist(), layout.TmpFFmpegProgress(), layout.TmpFFmpegAudioStats(), "", profile)
	starter := m.Starter
	if starter == nil {
		starter = ExecStarter{}
	}

	process, err := starter.Start(context.Background(), m.ffmpegBin(), args)
	if err != nil {
		return Snapshot{}, err
	}
	jobGeneration := uint64(0)
	if job.VideoCoverStart != nil {
		jobGeneration = job.VideoCoverStart.JobGeneration
	}
	graphGeneration, err := m.nextCoverGeneration(job.StreamID, jobGeneration)
	if err != nil {
		_ = process.Kill()
		_ = coverSource.Close()
		_ = watermarkSource.Close()
		return Snapshot{}, err
	}
	coverState := initialCoverRuntimeState(job.StreamID, graphGeneration, job.VideoCoverStart, watermarkState)
	if job.VideoCoverStart != nil {
		witnessCtx, cancelWitness := context.WithTimeout(context.Background(), m.coverApplyTimeout())
		witnessErr := m.coverGraphWitness().Apply(witnessCtx, coverSource, coverFrame, true, layout.TmpFFmpegProgress())
		cancelWitness()
		if witnessErr != nil {
			_ = process.Kill()
			_ = coverSource.Close()
			_ = watermarkSource.Close()
			return Snapshot{}, videocover.NewError(videocover.ErrorCoverGraphUnavailable)
		}
		markCoverApplied(&coverState, job.VideoCoverStart.Revision, job.VideoCoverStart.Active, job.VideoCoverStart.CoverAsset, watermarkState)
	}
	snapshot := Snapshot{
		StreamID:           job.StreamID,
		Name:               job.Name,
		Status:             "running",
		PID:                process.PID(),
		Archive:            lifecycle.ArchiveArtifactsForRun(job.StreamID, job.ArchiveRunID),
		StartedAtJST:       startedAt.In(jst()).Format(time.RFC3339),
		EncoderAudioGainDB: job.EncoderAudioGainDB,
		OverlayProfileID:   job.OverlayProfileID,
	}
	if err := writeStartMetadata(layout, job, snapshot, args, m.ffmpegBin(), outputRoute); err != nil {
		_ = process.Kill()
		_ = coverSource.Close()
		_ = watermarkSource.Close()
		return Snapshot{}, err
	}

	done := make(chan error, 1)
	m.mu.Lock()
	m.processes[job.StreamID] = &trackedProcess{
		snapshot: snapshot, process: process, job: job, done: done, watermark: watermarkSource,
		watermarkState: watermarkState, cover: coverSource, coverState: coverState,
		coverReplay: map[string]coverReplay{}, transparentCover: transparentCover,
		progressPath: layout.TmpFFmpegProgress(),
	}
	m.mu.Unlock()
	coverActive = false
	watermarkActive = false
	reservationActive = false

	log.Printf("encoder diagnostic: event=encoder.process.started stream_id=%s status=running encoder_profile_id=%s output_width=%d output_height=%d output_fps=%d youtube_output_mode=%s output_route=%s", job.StreamID, strings.TrimSpace(job.EncoderProfileID), profile.Width, profile.Height, profile.FPS, strings.TrimSpace(job.YouTubeOutputMode), outputRoute)
	m.report(observability.Signal{
		Type:      "event",
		Name:      "encoder.process.started",
		StreamID:  job.StreamID,
		Status:    "running",
		Timestamp: time.Now().UTC(),
		Attributes: map[string]any{
			"recording_mkv":       "final.mkv",
			"preview_playlist":    "preview/index.m3u8",
			"encoder_profile_id":  strings.TrimSpace(job.EncoderProfileID),
			"output_width":        profile.Width,
			"output_height":       profile.Height,
			"output_fps":          profile.FPS,
			"youtube_output_mode": strings.TrimSpace(job.YouTubeOutputMode),
			"output_route":        outputRoute,
		},
	})
	go m.wait(job.StreamID, process, done)
	go m.monitor(job.StreamID, layout.FinalMKV(), layout.TmpFFmpegProgress(), layout.TmpFFmpegAudioStats())
	return snapshot, nil
}

type RuntimeSettings struct {
	EncoderAudioGainDB float64
	OverlayProfileID   string
	OverlayConfig      map[string]any
}

func (m *Manager) UpdateRuntimeSettings(streamID string, settings RuntimeSettings) (Snapshot, error) {
	if strings.TrimSpace(streamID) == "" || math.IsNaN(settings.EncoderAudioGainDB) || math.IsInf(settings.EncoderAudioGainDB, 0) || settings.EncoderAudioGainDB < -60 || settings.EncoderAudioGainDB > 24 {
		return Snapshot{}, ErrInvalidRuntimeSettings
	}
	frame, err := watermarkfeed.Frame(settings.OverlayConfig)
	if err != nil {
		return Snapshot{}, errors.Join(ErrInvalidRuntimeSettings, err)
	}
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok || tracked.snapshot.Status != "running" || tracked.process == nil || tracked.watermark == nil {
		m.mu.Unlock()
		return Snapshot{}, ErrNotRunning
	}
	m.mu.Unlock()

	tracked.runtimeMu.Lock()
	defer tracked.runtimeMu.Unlock()
	argument := strconv.FormatFloat(settings.EncoderAudioGainDB, 'f', 1, 64) + "dB"
	commander, ok := tracked.process.(runtimeCommander)
	if !ok {
		return Snapshot{}, errors.New("ffmpeg runtime commands are unavailable")
	}
	if err := commander.Command("volume@gain", "volume", argument); err != nil {
		return Snapshot{}, err
	}
	// Serialize the two independently mutable visual inputs only for the short
	// witness window. This prevents a concurrent Cover apply from publishing a
	// witness that pairs its new Cover revision with a pre-witness Watermark.
	tracked.coverMu.Lock()
	witnessCtx, cancelWitness := context.WithTimeout(context.Background(), m.coverApplyTimeout())
	witnessErr := m.watermarkGraphWitness().Apply(witnessCtx, tracked.watermark, frame, tracked.progressPath)
	cancelWitness()
	if witnessErr != nil {
		tracked.markWatermarkWitnessUnknownLocked()
		tracked.coverMu.Unlock()
		return Snapshot{}, videocover.NewError(videocover.ErrorCoverGraphUnavailable)
	}
	tracked.watermarkMu.Lock()
	tracked.watermarkState.Revision++
	tracked.watermarkState.Enabled = watermarkEnabled(settings.OverlayConfig)
	if tracked.watermarkState.Enabled {
		tracked.watermarkState.VariantID = strings.TrimSpace(settings.OverlayProfileID)
	} else {
		tracked.watermarkState.VariantID = ""
	}
	watermarkState := tracked.watermarkState
	tracked.watermarkMu.Unlock()
	if tracked.coverState.JobGeneration != 0 {
		tracked.coverState.Watermark = watermarkState
		if tracked.coverState.AppliedWitness != nil {
			witness := *tracked.coverState.AppliedWitness
			witness.Watermark = watermarkState
			tracked.coverState.AppliedWitness = &witness
		}
	}
	tracked.coverMu.Unlock()

	m.mu.Lock()
	current, ok := m.processes[streamID]
	if !ok || current != tracked || current.snapshot.Status != "running" {
		m.mu.Unlock()
		return Snapshot{}, ErrNotRunning
	}
	current.job.EncoderAudioGainDB = settings.EncoderAudioGainDB
	current.job.OverlayProfileID = strings.TrimSpace(settings.OverlayProfileID)
	current.snapshot.EncoderAudioGainDB = settings.EncoderAudioGainDB
	current.snapshot.OverlayProfileID = strings.TrimSpace(settings.OverlayProfileID)
	snapshot := current.snapshot
	m.mu.Unlock()

	log.Printf("encoder diagnostic: event=encoder.runtime_settings.dispatched stream_id=%s status=dispatched audio_gain_db=%.1f overlay_profile_id=%s audio_command_written=true watermark_frame_updated=true", streamID, settings.EncoderAudioGainDB, strings.TrimSpace(settings.OverlayProfileID))
	m.report(observability.Signal{
		Type:      "event",
		Name:      "encoder.runtime_settings.dispatched",
		StreamID:  streamID,
		Status:    "dispatched",
		Timestamp: time.Now().UTC(),
		Attributes: map[string]any{
			"audio_gain_db":           settings.EncoderAudioGainDB,
			"overlay_profile_id":      strings.TrimSpace(settings.OverlayProfileID),
			"audio_command_written":   true,
			"watermark_frame_updated": true,
		},
	})
	return snapshot, nil
}

// VideoCoverState returns only a negotiated Cover runtime. Legacy starts that
// omitted video_cover_start remain transparently inactive and cannot be
// mutated through the capability path.
func (m *Manager) VideoCoverState(streamID string) (videocover.RuntimeState, error) {
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok || tracked.snapshot.Status != "running" || tracked.cover == nil {
		m.mu.Unlock()
		return videocover.RuntimeState{}, ErrNotRunning
	}
	m.mu.Unlock()

	tracked.coverMu.Lock()
	defer tracked.coverMu.Unlock()
	if tracked.coverState.JobGeneration == 0 {
		return videocover.RuntimeState{}, videocover.NewError(videocover.ErrorCapabilityRequired)
	}
	return tracked.coverStateSnapshot(), nil
}

// ApplyVideoCover performs a single fenced mutation. Asset failures are
// rejected before the feed changes. Once feed delivery begins, any missing
// graph witness is ambiguous and is never retried automatically; exact replay
// returns the stored response without another fetch or feed write.
func (m *Manager) ApplyVideoCover(ctx context.Context, streamID string, request videocover.ApplyRequest) (videocover.ApplyResponse, error) {
	if err := validateCoverApply(streamID, request); err != nil {
		return videocover.ApplyResponse{}, err
	}
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok {
		m.mu.Unlock()
		return videocover.ApplyResponse{}, ErrNotRunning
	}
	tracked.coverMu.Lock()
	running := tracked.snapshot.Status == "running" && tracked.cover != nil
	m.mu.Unlock()
	defer tracked.coverMu.Unlock()
	if !running {
		if state, exists := tracked.videoCoverRejectionStateLocked(); exists {
			switch {
			case request.JobGeneration != state.JobGeneration:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorStaleJobGeneration), nil
			case request.ExpectedGeneration != state.Generation:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorStaleCoverGeneration), nil
			case request.Revision < state.Desired.Revision:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorStaleCoverRevision), nil
			case request.Revision == state.Desired.Revision:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorRevisionPayloadConflict), nil
			}
			return rejectedCoverResponseFromState(terminalVideoCoverState(state), request, videocover.ErrorCoverGraphUnavailable), nil
		}
		return videocover.ApplyResponse{}, ErrNotRunning
	}
	if tracked.coverState.JobGeneration == 0 {
		return videocover.ApplyResponse{}, videocover.NewError(videocover.ErrorCapabilityRequired)
	}
	fingerprint := coverRequestFingerprint(request)
	if replay, exists := tracked.coverReplay[request.IdempotencyKey]; exists {
		if replay.fingerprint != fingerprint {
			return rejectedCoverResponse(tracked, request, videocover.ErrorIdempotencyConflict), nil
		}
		return replay.response, nil
	}
	if request.JobGeneration != tracked.coverState.JobGeneration {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorStaleJobGeneration)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	if request.ExpectedGeneration != tracked.coverState.Generation {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorStaleCoverGeneration)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	if request.Revision < tracked.coverState.Desired.Revision {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorStaleCoverRevision)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	if request.Revision == tracked.coverState.Desired.Revision {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorRevisionPayloadConflict)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}

	frame := tracked.transparentCover
	if request.Active {
		if m.CoverAssets == nil {
			response := rejectedCoverResponse(tracked, request, videocover.ErrorCapabilityRequired)
			tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
			return response, nil
		}
		fetchCtx, cancelFetch := context.WithTimeout(ctx, m.coverFetchTimeout())
		loaded, err := m.CoverAssets.Load(fetchCtx, streamID, *request.CoverAsset, tracked.job.EncoderProfile.Width, tracked.job.EncoderProfile.Height)
		cancelFetch()
		if err != nil {
			code := videocover.ErrorCodeOf(err)
			if code == videocover.ErrorInvalidRequest {
				return videocover.ApplyResponse{}, err
			}
			if code == "" {
				code = videocover.ErrorMediaAssetTimeout
			}
			response := rejectedCoverResponse(tracked, request, code)
			tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
			return response, nil
		}
		frame = loaded
	}

	lastGood := tracked.coverState.Applied
	if tracked.coverState.LastGoodApplied != nil {
		lastGood = *tracked.coverState.LastGoodApplied
	}
	tracked.coverState.Desired = desiredFromRequest(request)
	tracked.coverState.CoverAsset = nil
	if request.Active && request.CoverAsset != nil {
		asset := *request.CoverAsset
		tracked.coverState.CoverAsset = &asset
	}
	witnessCtx, cancelWitness := context.WithTimeout(ctx, m.coverApplyTimeout())
	witnessErr := m.coverGraphWitness().Apply(witnessCtx, tracked.cover, frame, false, tracked.progressPath)
	cancelWitness()
	watermark := tracked.currentWatermarkState()
	if witnessErr != nil {
		tracked.coverState.Readiness = videocover.ReadinessUnknown
		tracked.coverState.Applied = videocover.AppliedState{State: "unknown"}
		tracked.coverState.AppliedWitness = nil
		tracked.coverState.LastGoodApplied = &lastGood
		tracked.coverState.Error = &videocover.SafeError{Code: videocover.ErrorCoverApplyAmbiguous}
		tracked.coverState.Watermark = watermark
		response := videocover.ApplyResponse{
			StreamID: streamID, JobGeneration: request.JobGeneration, RequestedRevision: request.Revision,
			ActualGeneration: tracked.coverState.Generation, Accepted: true, Outcome: videocover.OutcomeAmbiguous,
			Actual: tracked.coverState, Error: &videocover.SafeError{Code: videocover.ErrorCoverApplyAmbiguous},
		}
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	markCoverApplied(&tracked.coverState, request.Revision, request.Active, request.CoverAsset, watermark)
	response := videocover.ApplyResponse{
		StreamID: streamID, JobGeneration: request.JobGeneration, RequestedRevision: request.Revision,
		ActualGeneration: tracked.coverState.Generation, Accepted: true, Applied: true,
		Outcome: videocover.OutcomeApplied, Actual: tracked.coverState,
	}
	tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
	return response, nil
}

func validateCoverStart(snapshot videocover.StartSnapshot) error {
	if snapshot.JobGeneration < 1 || snapshot.Revision < 1 || !validIdempotencyKey(snapshot.IdempotencyKey) {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	if snapshot.Active && snapshot.CoverAsset == nil || !snapshot.Active && snapshot.CoverAsset != nil {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	if snapshot.CoverAsset != nil {
		if err := videocover.ValidateDescriptor(*snapshot.CoverAsset); err != nil {
			return err
		}
	}
	return nil
}

func validateCoverApply(streamID string, request videocover.ApplyRequest) error {
	if strings.TrimSpace(streamID) == "" || request.StreamID != streamID || request.JobGeneration < 1 || request.ExpectedGeneration < 1 || request.Revision < 1 || !validIdempotencyKey(request.IdempotencyKey) {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	if request.Active {
		if request.CoverAsset == nil || request.HideConfirmed {
			return videocover.NewError(videocover.ErrorInvalidRequest)
		}
		if err := videocover.ValidateDescriptor(*request.CoverAsset); err != nil {
			return err
		}
	} else if request.CoverAsset != nil || !request.HideConfirmed {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	return nil
}

func validIdempotencyKey(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	length := utf8.RuneCountInString(value)
	if length < 1 || length > 128 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(value)
	last, _ := utf8.DecodeLastRuneInString(value)
	if idempotencyEdgeSpace(first) || idempotencyEdgeSpace(last) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func idempotencyEdgeSpace(character rune) bool {
	return unicode.IsSpace(character) || character == '\ufeff'
}

func initialCoverRuntimeState(streamID string, generation uint64, snapshot *videocover.StartSnapshot, watermark videocover.LayerState) videocover.RuntimeState {
	state := videocover.RuntimeState{
		StreamID: streamID, Generation: generation, Capability: videocover.Capability,
		Readiness: videocover.ReadinessNotReady, Applied: videocover.AppliedState{State: "unknown"},
		Cover: videocover.LayerState{Revision: 1}, Watermark: watermark,
		Pipeline: videocover.FixedPipelineInvariant(), NoAutomaticResend: true,
		Error: &videocover.SafeError{Code: videocover.ErrorCapabilityRequired},
	}
	if snapshot == nil {
		return state
	}
	state.JobGeneration = snapshot.JobGeneration
	state.Desired = videocover.DesiredState{Active: snapshot.Active, Revision: snapshot.Revision, Source: "none"}
	if snapshot.Active && snapshot.CoverAsset != nil {
		state.Desired.Source = "upload"
		state.Desired.VariantID = snapshot.CoverAsset.VariantID
	}
	state.Error = &videocover.SafeError{Code: videocover.ErrorCoverGraphUnavailable}
	return state
}

func markCoverApplied(state *videocover.RuntimeState, revision uint64, active bool, asset *videocover.MediaAssetDescriptor, watermark videocover.LayerState) {
	if state == nil {
		return
	}
	knownActive := active
	variantID := ""
	state.CoverAsset = nil
	if active && asset != nil {
		copyAsset := *asset
		state.CoverAsset = &copyAsset
		variantID = asset.VariantID
	}
	state.Desired = videocover.DesiredState{Active: active, Revision: revision, Source: "none", VariantID: variantID}
	if active {
		state.Desired.Source = "upload"
	}
	state.Applied = videocover.AppliedState{State: "known", Active: &knownActive, Revision: revision, VariantID: variantID}
	state.Cover = videocover.LayerState{Enabled: active, Revision: revision, VariantID: variantID}
	state.Watermark = watermark
	state.Readiness = videocover.ReadinessReady
	state.Error = nil
	state.LastGoodApplied = nil
	state.AppliedWitness = &videocover.AppliedWitness{
		GraphApplied: true, Generation: state.Generation, Revision: revision, Active: active,
		Cover: state.Cover, Watermark: watermark, Pipeline: videocover.FixedPipelineInvariant(),
	}
}

func desiredFromRequest(request videocover.ApplyRequest) videocover.DesiredState {
	desired := videocover.DesiredState{Active: request.Active, Revision: request.Revision, Source: "none"}
	if request.Active && request.CoverAsset != nil {
		desired.Source = "upload"
		desired.VariantID = request.CoverAsset.VariantID
	}
	return desired
}

func rejectedCoverResponse(tracked *trackedProcess, request videocover.ApplyRequest, code videocover.ErrorCode) videocover.ApplyResponse {
	return rejectedCoverResponseFromState(tracked.coverStateSnapshot(), request, code)
}

func rejectedCoverResponseFromState(state videocover.RuntimeState, request videocover.ApplyRequest, code videocover.ErrorCode) videocover.ApplyResponse {
	safeError := &videocover.SafeError{Code: code}
	if isCoverGraphOrAssetError(code) {
		// Rejection does not mutate the authoritative graph state. The response
		// still reports this operation as not-ready with the exact safe error,
		// as required by the cross-repository response contract.
		state.Readiness = videocover.ReadinessNotReady
		state.Error = safeError
	}
	return videocover.ApplyResponse{
		StreamID: state.StreamID, JobGeneration: state.JobGeneration, RequestedRevision: request.Revision,
		ActualGeneration: state.Generation, Rejected: true, Outcome: videocover.OutcomeRejected,
		Actual: state, Error: safeError,
	}
}

// VideoCoverRejection returns a contract response only when the manager still
// has an authoritative negotiated runtime snapshot. It deliberately refuses to
// derive job/generation state from the rejected request.
func (m *Manager) VideoCoverRejection(streamID string, request videocover.ApplyRequest, code videocover.ErrorCode) (videocover.ApplyResponse, bool) {
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok {
		m.mu.Unlock()
		return videocover.ApplyResponse{}, false
	}
	tracked.coverMu.Lock()
	m.mu.Unlock()
	defer tracked.coverMu.Unlock()
	state, ok := tracked.videoCoverRejectionStateLocked()
	if !ok {
		return videocover.ApplyResponse{}, false
	}
	return rejectedCoverResponseFromState(state, request, code), true
}

func isCoverGraphOrAssetError(code videocover.ErrorCode) bool {
	switch code {
	case videocover.ErrorMediaAssetUnauthorized,
		videocover.ErrorMediaAssetNotFound,
		videocover.ErrorMediaAssetHashMismatch,
		videocover.ErrorMediaAssetDimensionMismatch,
		videocover.ErrorMediaAssetTimeout,
		videocover.ErrorMediaAssetFormatUnsupported,
		videocover.ErrorMediaAssetTooLarge,
		videocover.ErrorMediaAssetDecodeFailed,
		videocover.ErrorMediaAssetAspectRatioInvalid,
		videocover.ErrorMediaAssetVariantProcessing,
		videocover.ErrorMediaAssetVariantFailed,
		videocover.ErrorCoverGraphUnavailable,
		videocover.ErrorCapabilityRequired:
		return true
	default:
		return false
	}
}

func coverRequestFingerprint(request videocover.ApplyRequest) [32]byte {
	body, _ := json.Marshal(request)
	return sha256.Sum256(body)
}

func (tracked *trackedProcess) storeCoverReplay(key string, replay coverReplay) {
	if tracked.coverReplay == nil {
		tracked.coverReplay = map[string]coverReplay{}
	}
	if _, exists := tracked.coverReplay[key]; !exists {
		tracked.coverReplayOrder = append(tracked.coverReplayOrder, key)
	}
	tracked.coverReplay[key] = replay
	for len(tracked.coverReplayOrder) > maxCoverReplayEntries {
		oldest := tracked.coverReplayOrder[0]
		tracked.coverReplayOrder = tracked.coverReplayOrder[1:]
		delete(tracked.coverReplay, oldest)
	}
}

func initialWatermarkState(profileID string, config map[string]any) videocover.LayerState {
	state := videocover.LayerState{Enabled: watermarkEnabled(config), Revision: 1}
	if state.Enabled {
		state.VariantID = strings.TrimSpace(profileID)
	}
	return state
}

func watermarkEnabled(config map[string]any) bool {
	enabled, ok := config["watermark_enabled"].(bool)
	return ok && enabled
}

func (tracked *trackedProcess) currentWatermarkState() videocover.LayerState {
	tracked.watermarkMu.Lock()
	defer tracked.watermarkMu.Unlock()
	return tracked.watermarkState
}

// markWatermarkWitnessUnknownLocked requires coverMu. The previous known Cover
// remains as last-good state, but the combined graph witness is no longer safe
// after a Watermark delivery whose downstream effect could not be observed.
func (tracked *trackedProcess) markWatermarkWitnessUnknownLocked() {
	if tracked.coverState.JobGeneration == 0 {
		return
	}
	if tracked.coverState.Applied.State == "known" {
		lastGood := tracked.coverState.Applied
		tracked.coverState.LastGoodApplied = &lastGood
	}
	tracked.coverState.Readiness = videocover.ReadinessUnknown
	tracked.coverState.Applied = videocover.AppliedState{State: "unknown"}
	tracked.coverState.AppliedWitness = nil
	tracked.coverState.Error = &videocover.SafeError{Code: videocover.ErrorCoverGraphUnavailable}
	tracked.coverState.Watermark = tracked.currentWatermarkState()
}

// coverStateSnapshot binds the independently current Watermark observation to
// both runtime state and its graph witness without changing any Cover-owned
// revision or desired/applied field. Exact idempotency replays deliberately use
// their stored historical response instead.
func (tracked *trackedProcess) coverStateSnapshot() videocover.RuntimeState {
	state := tracked.coverState
	watermark := tracked.currentWatermarkState()
	state.Watermark = watermark
	if state.AppliedWitness != nil {
		witness := *state.AppliedWitness
		witness.Watermark = watermark
		state.AppliedWitness = &witness
	}
	return state
}

func (tracked *trackedProcess) videoCoverRejectionStateLocked() (videocover.RuntimeState, bool) {
	state := tracked.coverStateSnapshot()
	if state.StreamID != "" && state.JobGeneration > 0 && state.Generation > 0 {
		return state, true
	}
	if tracked.terminalCoverState == nil {
		return videocover.RuntimeState{}, false
	}
	state = *tracked.terminalCoverState
	return state, state.StreamID != "" && state.JobGeneration > 0 && state.Generation > 0
}

func terminalVideoCoverState(state videocover.RuntimeState) videocover.RuntimeState {
	if state.Applied.State == "known" {
		lastGood := state.Applied
		state.LastGoodApplied = &lastGood
	}
	state.Readiness = videocover.ReadinessNotReady
	state.Applied = videocover.AppliedState{State: "unknown"}
	state.AppliedWitness = nil
	state.Error = &videocover.SafeError{Code: videocover.ErrorCoverGraphUnavailable}
	return state
}

func (m *Manager) coverApplyTimeout() time.Duration {
	if m.CoverApplyTimeout > 0 {
		return m.CoverApplyTimeout
	}
	return 6 * time.Second
}

func (m *Manager) coverFetchTimeout() time.Duration {
	if m.CoverFetchTimeout > 0 {
		return m.CoverFetchTimeout
	}
	return 10 * time.Second
}

func (m *Manager) coverGraphWitness() CoverGraphWitness {
	if m.CoverWitness != nil {
		return m.CoverWitness
	}
	return progressCoverWitness{}
}

func (m *Manager) watermarkGraphWitness() WatermarkGraphWitness {
	if m.WatermarkWitness != nil {
		return m.WatermarkWitness
	}
	return progressWatermarkWitness{}
}

type progressCoverWitness struct{}

func (progressCoverWitness) Apply(ctx context.Context, source *imagefeed.Source, frame []byte, initial bool, progressPath string) error {
	var err error
	if initial {
		err = source.WaitInitialDelivery(ctx)
	} else {
		err = source.UpdateAndWait(ctx, frame)
	}
	if err != nil {
		return err
	}
	return waitForVisualOutputAdvance(ctx, progressPath)
}

type progressWatermarkWitness struct{}

func (progressWatermarkWitness) Apply(ctx context.Context, source *watermarkfeed.Source, frame []byte, progressPath string) error {
	if err := source.UpdateAndWait(ctx, frame); err != nil {
		return err
	}
	return waitForVisualOutputAdvance(ctx, progressPath)
}

func waitForVisualOutputAdvance(ctx context.Context, progressPath string) error {
	// Establish the output baseline only after the exact feed version was
	// delivered. Progress that happened while the socket write was pending is
	// not evidence that this visual-layer revision crossed the graph.
	before := readCoverProgress(progressPath)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := readCoverProgress(progressPath)
		if current.Frame > before.Frame && current.OutTimeUS > before.OutTimeUS {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func readCoverProgress(path string) ffmpeg.Progress {
	body, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ffmpeg.Progress{}
	}
	return ffmpeg.ParseProgress(string(body))
}

// OutputRelayPolicy returns the non-secret routing configuration.  An unset
// mode with a configured URL is deliberately interpreted as the legacy fixed
// stream-key Relay so existing host/systemd environments keep working.
func (m *Manager) OutputRelayPolicy() outputrelay.Policy {
	return outputrelay.NewWithRequireRelay(m.OutputRelayURL, m.OutputRelayMode, m.OutputRelayBindingID, m.RequireOutputRelay)
}

// AuthorizeOutputRelay must be called before resolving a YouTube key, an input
// target, or starting FFmpeg.  It is shared with the HTTP layer to keep the
// direct, legacy, and static-Live-API acceptance rules identical.
func (m *Manager) AuthorizeOutputRelay(job lifecycle.StreamJob) (bool, error) {
	return m.OutputRelayPolicy().AuthorizeYouTubeOutput(job.YouTubeOutputMode, job.YouTubeOutputReady, job.OutputRelayBindingID)
}

func clearUnusedYouTubeOutputTarget(job *lifecycle.StreamJob) {
	if job == nil {
		return
	}
	job.RTMPURL = ""
	job.StreamKey = ""
	job.StreamKeySecretName = ""
}

func (m *Manager) validateInputForLayout(job lifecycle.StreamJob, layout archive.Layout) error {
	if job.InputMode == "worker_scene_frames_srt" {
		if !strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_worker_video:") {
			return ffmpeg.ErrUnsafeInputTarget
		}
		if !strings.HasPrefix(strings.TrimSpace(job.AudioInputURL), "internal_discord_audio:") {
			return ffmpeg.ErrUnsafeInputTarget
		}
		got := filepath.Clean(ffmpeg.ResolveInputTarget(job.AudioInputURL))
		want := filepath.Clean(layout.TmpDiscordOpusSDP())
		if got != want {
			return ffmpeg.ErrUnsafeInputTarget
		}
		return nil
	}
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
	if strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_worker_video:") || strings.TrimSpace(job.AudioInputURL) != "" {
		return ffmpeg.ErrUnsafeInputTarget
	}
	return nil
}

func (m *Manager) liveOutputTarget(job lifecycle.StreamJob) (string, error) {
	policy := m.OutputRelayPolicy()
	if err := policy.ValidateConfiguration(); err != nil {
		return "", err
	}
	if !policy.UsesLocalRelay() {
		if m.RequireOutputRelay {
			return "", errors.New("output relay URL is required")
		}
		return job.RTMPURL + "/" + job.StreamKey, nil
	}
	target := relayOutputTarget(policy.URL, job.StreamID)
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
	if !ok {
		m.mu.Unlock()
		alreadyStopped, receiptErr := m.hasStopReceipt(streamID)
		if receiptErr != nil {
			return Snapshot{}, receiptErr
		}
		if alreadyStopped {
			return Snapshot{StreamID: streamID, Status: "stopped"}, ErrAlreadyStopped
		}
		return Snapshot{}, ErrNotRunning
	}
	if tracked.snapshot.Status != "running" {
		snapshot := tracked.snapshot
		m.mu.Unlock()
		switch snapshot.Status {
		case "stopping", "packaging", "stopped", "failed", "package_failed":
			return snapshot, ErrAlreadyStopped
		case "starting":
			return snapshot, ErrStarting
		default:
			return Snapshot{}, ErrNotRunning
		}
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
	if err := m.recordStopReceipt(streamID); err != nil {
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
		if err := process.Kill(); err != nil {
			return err
		}
		// A durable stop receipt must only be written after the process wait has
		// observed exit. Killing a process is not itself proof that it is gone.
		select {
		case <-done:
			return nil
		case <-time.After(stopGracePeriod()):
			return errors.New("stream process did not exit after kill")
		}
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
	stopRequested := tracked.snapshot.Status == "stopping"
	terminationAttributes := map[string]any{
		"error_class":         "clean_exit",
		"stop_requested":      stopRequested,
		"stderr_tail_present": false,
	}
	if stopRequested {
		terminationAttributes["error_class"] = "stop_requested"
	}
	stderr := processStderr(process)
	if err != nil {
		redactedError, errorClass := redactedProcessExit(err, stderr, tracked.job)
		terminationAttributes["error"] = redactedError
		terminationAttributes["error_class"] = errorClass
		if exitCode, ok := processExitCode(err); ok {
			terminationAttributes["exit_code"] = exitCode
		}
		if stderr != "" {
			if safeStderr := redactedProcessStderr(stderr, tracked.job); safeStderr != "" {
				terminationAttributes["stderr_tail"] = safeStderr
				terminationAttributes["stderr_tail_present"] = true
			}
		}
	}
	if err == nil && stderr != "" {
		if safeStderr := redactedProcessStderr(stderr, tracked.job); safeStderr != "" {
			terminationAttributes["stderr_tail"] = safeStderr
			terminationAttributes["stderr_tail_present"] = true
		}
	}
	if stopRequested {
		if safeStderr := redactedProcessStderr(stderr, tracked.job); safeStderr != "" {
			if errorClass := classifyStoppedProcessFailure(safeStderr); errorClass != "" {
				// A graceful q/stop can still leave an archive tee slave or an
				// output writer failed. Keep the package/fallback path alive, but
				// expose the failure instead of reporting a clean stop.
				terminationAttributes["error_class"] = errorClass
				terminationAttributes["process_error"] = true
				terminationAttributes["archive_partial"] = true
				if _, ok := terminationAttributes["error"]; !ok {
					terminationAttributes["error"] = "process stopped with output failure"
				}
			}
		}
	}
	addProcessOutputDiagnostics(terminationAttributes, m.archiveRoot(), streamID)
	if err != nil && !stopRequested {
		tracked.snapshot.Status = "failed"
		tracked.snapshot.Error = terminationAttributes["error"].(string)
		signal = observability.Signal{
			Type:       "error",
			Name:       "encoder.process.exited",
			StreamID:   streamID,
			Status:     "failed",
			Timestamp:  time.Now().UTC(),
			Attributes: terminationAttributes,
		}
	} else {
		shouldPackage = m.Packager != nil
		if shouldPackage {
			tracked.snapshot.Status = "packaging"
		} else {
			tracked.snapshot.Status = "stopped"
		}
		packageJob = lifecycle.PackageJob{StreamID: tracked.job.StreamID, ArchiveRunID: tracked.job.ArchiveRunID, Name: tracked.job.Name, StartedAt: tracked.job.StartedAt, ArchiveConfig: tracked.job.ArchiveConfig}
		signal = observability.Signal{
			Type:       "event",
			Name:       "encoder.process.stopped",
			StreamID:   streamID,
			Status:     "stopped",
			Timestamp:  time.Now().UTC(),
			Attributes: terminationAttributes,
		}
	}
	scrubTrackedProcessJob(tracked)
	watermarkSource := tracked.watermark
	tracked.watermark = nil
	tracked.coverMu.Lock()
	coverSource := tracked.cover
	tracked.cover = nil
	tracked.coverMu.Unlock()
	m.mu.Unlock()
	if coverSource != nil {
		_ = coverSource.Close()
	}
	if watermarkSource != nil {
		_ = watermarkSource.Close()
	}
	if m.ProcessExitHook != nil {
		m.ProcessExitHook(streamID)
	}
	logProcessDiagnostic(signal)
	m.report(signal)
	m.reportMetric(streamID, "encoder.process_alive", 0)
	if shouldPackage {
		m.packageArchive(packageJob)
	}
}

type stderrProvider interface {
	Stderr() string
}

func processStderr(process RunningProcess) string {
	provider, ok := process.(stderrProvider)
	if !ok {
		return ""
	}
	return strings.TrimSpace(provider.Stderr())
}

func processExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	code := exitErr.ExitCode()
	return code, code >= 0
}

func redactedProcessStderr(stderr string, job lifecycle.StreamJob) string {
	return strings.TrimSpace(redaction.Diagnostic(stderr,
		job.StreamKey,
		job.InputURL,
		job.AudioInputURL,
		job.CoverInputURL,
		job.WatermarkInputURL,
		job.RTMPURL,
		job.RTMPURL+"/"+job.StreamKey,
	))
}

func redactedProcessExit(err error, stderr string, job lifecycle.StreamJob) (string, string) {
	base := redaction.Diagnostic(err.Error(),
		job.StreamKey,
		job.InputURL,
		job.AudioInputURL,
		job.CoverInputURL,
		job.WatermarkInputURL,
		job.RTMPURL,
		job.RTMPURL+"/"+job.StreamKey,
	)
	safeStderr := redactedProcessStderr(stderr, job)
	if safeStderr != "" {
		base = strings.TrimSpace(base + ": " + safeStderr)
	}
	return base, classifyProcessExit(safeStderr)
}

func classifyProcessExit(stderr string) string {
	lower := strings.ToLower(stderr)
	switch {
	case lower == "":
		return "process_exit"
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "timed out"),
		strings.Contains(lower, "network is unreachable"):
		return "transport"
	case strings.Contains(lower, "error opening output"),
		strings.Contains(lower, "failed to open output"),
		strings.Contains(lower, "permission denied"):
		return "output_init"
	case strings.Contains(lower, "error initializing filter"),
		strings.Contains(lower, "failed to configure filter"):
		return "filter_init"
	case strings.Contains(lower, "invalid data found"),
		strings.Contains(lower, "error while decoding"),
		strings.Contains(lower, "could not find codec"):
		return "input_decode"
	case strings.Contains(lower, "rtmp"),
		strings.Contains(lower, "rtmps"),
		strings.Contains(lower, "handshake"):
		return "relay"
	default:
		return "ffmpeg_exit"
	}
}

// classifyStoppedProcessFailure recognizes failures that can be emitted by
// FFmpeg while the controller is already asking the process to stop. A
// non-zero Wait error is not required here: tee/output failures may leave the
// process with a normal-looking stop result while the archive slave is
// already unusable.
func classifyStoppedProcessFailure(stderr string) string {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "slave muxer") && strings.Contains(lower, "failed"):
		return "archive_output"
	case strings.Contains(lower, "error writing header"),
		strings.Contains(lower, "error writing trailer"),
		strings.Contains(lower, "error while filtering"),
		strings.Contains(lower, "error during encoding"),
		strings.Contains(lower, "conversion failed"):
		return "archive_output"
	default:
		return ""
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
		m.reportArchiveFileMetrics(job)
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
		if m.ArtifactReporter != nil {
			layout, layoutErr := archiveLayoutForPackage(m.ArchiveRoot, job)
			if layoutErr == nil {
				artifacts := control.ArchiveArtifacts(layout)
				finalMP4Exists := false
				for _, artifact := range artifacts {
					if artifact.Name == "final.mp4" {
						finalMP4Exists = true
						break
					}
				}
				if finalMP4Exists {
					reportCtx, cancelReport := context.WithTimeout(context.Background(), m.artifactReportTimeout())
					reportErr := m.ArtifactReporter.ReportArtifacts(reportCtx, job.StreamID, artifacts, artifactReportArchiveRuns(job)...)
					cancelReport()
					if reportErr != nil {
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
		}
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
	m.reportArchiveFileMetrics(job)
	m.reportMetric(job.StreamID, "archive.package_status", 1)
	m.reportMetric(job.StreamID, "archive.package_partial", boolMetric(result.Partial))
	m.reportMetric(job.StreamID, "archive.final_mkv_usable", boolMetric(result.ArchiveSource == "final_mkv"))
	m.reportMetric(job.StreamID, "archive.final_mp4_exists", 1)
	m.reportMetric(job.StreamID, "recorder.remux_duration_ms", result.RemuxDurationMS)
	m.reportMetric(job.StreamID, "gdrive.upload_status", 1)
	m.reportMetric(job.StreamID, "gdrive.upload_retry_count", float64(maxInt(result.Metadata.Upload.Attempts-1, 0)))
	m.reportMetric(job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
	m.reportMetric(job.StreamID, "gdrive.upload_file_count", float64(result.Metadata.Upload.UploadedFileCount()))
	m.reportMetric(job.StreamID, "gdrive.upload_folder_fingerprint_present", boolMetric(result.Metadata.Upload.HasFolderFingerprint()))
	m.reportMetric(job.StreamID, "gdrive.upload_final_mp4_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("final.mp4")))
	m.reportMetric(job.StreamID, "gdrive.upload_metadata_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("metadata.json")))
	packageAttributes := map[string]any{
		"upload_dry_run":    result.Metadata.Upload.DryRun,
		"upload_attempts":   result.Metadata.Upload.Attempts,
		"file_count":        len(result.Metadata.Upload.FileIDs),
		"remux_duration_ms": result.RemuxDurationMS,
		"archive_source":    result.ArchiveSource,
		"archive_partial":   result.Partial,
	}
	m.report(observability.Signal{
		Type:       "event",
		Name:       "archive.package.completed",
		StreamID:   job.StreamID,
		Status:     "completed",
		Timestamp:  time.Now().UTC(),
		Attributes: packageAttributes,
	})
	if result.Partial {
		m.report(observability.Signal{
			Type:      "warning",
			Name:      "archive.package.partial",
			StreamID:  job.StreamID,
			Status:    "warning",
			Timestamp: time.Now().UTC(),
			Attributes: map[string]any{
				"archive_source":  result.ArchiveSource,
				"archive_partial": true,
				"error_class":     "archive_source_fallback",
			},
		})
	}
	if m.ArtifactReporter != nil {
		artifacts := control.ArchiveArtifacts(result.Layout)
		if len(artifacts) > 0 {
			reportCtx, cancelReport := context.WithTimeout(context.Background(), m.artifactReportTimeout())
			err := m.ArtifactReporter.ReportArtifacts(reportCtx, job.StreamID, artifacts, artifactReportArchiveRuns(job)...)
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
		tracked.snapshot.Archive["final_artifact_set"] = lifecycle.ArchiveArtifactsForRun(job.StreamID, job.ArchiveRunID)["final_artifact_set"]
		tracked.snapshot.Archive["final_mp4"] = "final.mp4"
		tracked.snapshot.Archive["archive_source"] = result.ArchiveSource
		tracked.snapshot.Archive["archive_partial"] = strconv.FormatBool(result.Partial)
		scrubTrackedProcessJob(tracked)
	}
	m.mu.Unlock()
}

func archiveLayoutForPackage(root string, job lifecycle.PackageJob) (archive.Layout, error) {
	if strings.TrimSpace(job.ArchiveRunID) == "" {
		return archive.NewLayout(root, job.StreamID)
	}
	return archive.NewRunLayout(root, job.StreamID, job.ArchiveRunID)
}

func artifactReportArchiveRuns(job lifecycle.PackageJob) []control.ArchiveRun {
	if strings.TrimSpace(job.ArchiveRunID) == "" {
		return nil
	}
	return []control.ArchiveRun{{ID: job.ArchiveRunID, StartedAt: job.StartedAt}}
}

func scrubTrackedProcessJob(tracked *trackedProcess) {
	if tracked == nil {
		return
	}
	tracked.coverMu.Lock()
	defer tracked.coverMu.Unlock()
	if state, ok := tracked.videoCoverRejectionStateLocked(); ok {
		state = terminalVideoCoverState(state)
		tracked.terminalCoverState = &state
	}
	job := tracked.job
	job.InputURL = ""
	job.AudioInputURL = ""
	job.CoverInputURL = ""
	job.WatermarkInputURL = ""
	job.VideoCoverStart = nil
	job.RTMPURL = ""
	job.StreamKey = ""
	job.ArchiveConfig.FolderID = ""
	job.ArchiveConfig.ServiceAccountJSON = ""
	job.ArchiveConfig.ClientSecret = ""
	job.ArchiveConfig.RefreshToken = ""
	tracked.job = job
	tracked.transparentCover = nil
	tracked.coverReplay = nil
	tracked.coverReplayOrder = nil
	tracked.coverState = videocover.RuntimeState{}
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

func (m *Manager) reportArchiveFileMetrics(job lifecycle.PackageJob) {
	layout, err := archiveLayoutForPackage(m.archiveRoot(), job)
	if err != nil {
		return
	}
	streamID := job.StreamID
	if fileSize(layout.FinalMKV()) > 0 {
		m.reportMetric(streamID, "archive.final_mkv_exists", 1)
	} else {
		m.reportMetric(streamID, "archive.final_mkv_exists", 0)
	}
	m.reportMetric(streamID, "archive.final_mkv_bytes", float64(fileSize(layout.FinalMKV())))
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
	if err := m.Reporter.Report(context.Background(), signal); err != nil && isProcessDiagnosticSignal(signal.Name) {
		log.Printf("encoder diagnostic report failed: event=%s stream_id=%s error_class=observability_request_failed", signal.Name, signal.StreamID)
	}
}

func isProcessDiagnosticSignal(name string) bool {
	return strings.HasPrefix(name, "encoder.process.")
}

func logProcessDiagnostic(signal observability.Signal) {
	errorClass, _ := signal.Attributes["error_class"].(string)
	exitCode, hasExitCode := signal.Attributes["exit_code"]
	stderrTail, _ := signal.Attributes["stderr_tail"].(string)
	stopRequested, _ := signal.Attributes["stop_requested"].(bool)
	stderrTailPresent, _ := signal.Attributes["stderr_tail_present"].(bool)
	progress, _ := signal.Attributes["ffmpeg_progress"].(string)
	progressPresent, _ := signal.Attributes["ffmpeg_progress_present"].(bool)
	frame := signal.Attributes["ffmpeg_frame"]
	outTimeUS := signal.Attributes["ffmpeg_out_time_us"]
	fps := signal.Attributes["ffmpeg_fps"]
	speedRatio := signal.Attributes["ffmpeg_speed_ratio"]
	finalMKVBytes := signal.Attributes["final_mkv_bytes"]
	finalMKVPresent, _ := signal.Attributes["final_mkv_present"].(bool)
	log.Printf("encoder diagnostic: event=%s stream_id=%s status=%s error_class=%s stop_requested=%t exit_code_present=%t exit_code=%v stderr_tail_present=%t stderr_tail=%q ffmpeg_progress_present=%t ffmpeg_progress=%s ffmpeg_frame=%v ffmpeg_out_time_us=%v ffmpeg_fps=%v ffmpeg_speed_ratio=%v final_mkv_present=%t final_mkv_bytes=%v", signal.Name, signal.StreamID, signal.Status, errorClass, stopRequested, hasExitCode, exitCode, stderrTailPresent, stderrTail, progressPresent, progress, frame, outTimeUS, fps, speedRatio, finalMKVPresent, finalMKVBytes)
}

func addProcessOutputDiagnostics(attributes map[string]any, archiveRoot, streamID string) {
	attributes["ffmpeg_progress_present"] = false
	attributes["ffmpeg_frame"] = int64(0)
	attributes["ffmpeg_out_time_us"] = int64(0)
	attributes["ffmpeg_fps"] = float64(0)
	attributes["ffmpeg_speed_ratio"] = float64(0)
	attributes["ffmpeg_progress"] = ""
	attributes["final_mkv_present"] = false
	attributes["final_mkv_bytes"] = int64(0)

	layout, err := archive.NewLayout(archiveRoot, streamID)
	if err != nil {
		return
	}
	if body, err := os.ReadFile(layout.TmpFFmpegProgress()); err == nil {
		progress := ffmpeg.ParseProgress(string(body))
		attributes["ffmpeg_progress_present"] = true
		attributes["ffmpeg_frame"] = progress.Frame
		attributes["ffmpeg_out_time_us"] = progress.OutTimeUS
		attributes["ffmpeg_fps"] = progress.FPS
		attributes["ffmpeg_speed_ratio"] = progress.SpeedRatio
		attributes["ffmpeg_progress"] = progress.Progress
	}
	if info, err := os.Stat(layout.FinalMKV()); err == nil {
		attributes["final_mkv_present"] = true
		attributes["final_mkv_bytes"] = info.Size()
	}
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
	if progress.SpeedRatio > 0 {
		m.reportMetric(streamID, "encoder.output_speed_ratio", progress.SpeedRatio)
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

func writeStartMetadata(layout archive.Layout, job lifecycle.StreamJob, snapshot Snapshot, args []string, bin, outputRoute string) error {
	extra := map[string]any{"live_process": true}
	if archiveRunID := strings.TrimSpace(job.ArchiveRunID); archiveRunID != "" {
		extra["archive_run_id"] = archiveRunID
	}
	if job.InputMode != "" {
		extra["input_mode"] = job.InputMode
	}
	if strings.TrimSpace(job.EncoderProfileID) != "" {
		extra["encoder_profile_id"] = strings.TrimSpace(job.EncoderProfileID)
	}
	if job.EncoderProfile.Width > 0 {
		extra["output_width"] = job.EncoderProfile.Width
		extra["output_height"] = job.EncoderProfile.Height
		extra["output_fps"] = job.EncoderProfile.FPS
	}
	if youtubeOutputMode := strings.TrimSpace(job.YouTubeOutputMode); youtubeOutputMode != "" {
		extra["youtube_output_mode"] = youtubeOutputMode
	}
	if outputRoute = strings.TrimSpace(outputRoute); outputRoute == "direct" || outputRoute == "local_relay" {
		extra["output_route"] = outputRoute
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
	return archive.DryRunUploader{}
}

func jst() *time.Location {
	return time.FixedZone("JST", 9*60*60)
}
