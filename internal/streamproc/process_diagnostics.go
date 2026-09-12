package streamproc

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/redaction"
)

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
