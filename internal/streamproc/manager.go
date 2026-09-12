package streamproc

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
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
