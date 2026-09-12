package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

type PackageError struct {
	Phase string
	Err   error
}

var ErrPackageInProgress = errors.New("archive package already in progress")

var packageLocks = struct {
	sync.Mutex
	active map[string]struct{}
}{active: map[string]struct{}{}}

func (e PackageError) Error() string {
	if e.Err == nil {
		return e.Phase
	}
	return e.Err.Error()
}

func (e PackageError) Unwrap() error {
	return e.Err
}

func ErrorPhase(err error) string {
	var packageErr PackageError
	if errors.As(err, &packageErr) {
		return packageErr.Phase
	}
	return ""
}

func ErrorClass(err error) string {
	switch ErrorPhase(err) {
	case "input":
		return "archive_input_unavailable"
	case "remux":
		return "ffmpeg_remux_failed"
	case "package":
		return "archive_package_failed"
	case "upload":
		return "archive_upload_failed"
	case "retention":
		return "archive_retention_failed"
	default:
		if err == nil {
			return "none"
		}
		return "operation_failed"
	}
}

func SafeErrorSummary(err error) string {
	phase := ErrorPhase(err)
	class := ErrorClass(err)
	if phase == "" {
		phase = "unknown"
	}
	if class == "" {
		class = "operation_failed"
	}
	return phase + ":" + class
}

func (m Manager) Package(ctx context.Context, job PackageJob) (Result, error) {
	if job.StreamID == "" || job.Name == "" {
		return Result{}, errors.New("stream id and name are required")
	}
	if strings.TrimSpace(job.ArchiveRunID) == "" || job.StartedAt.IsZero() {
		return Result{}, errors.New("archive_run_id and started_at are required")
	}
	layout, err := archiveLayout(m.ArchiveRoot, job.StreamID, job.ArchiveRunID)
	if err != nil {
		return Result{}, err
	}
	release, err := acquirePackageLock(layout.RootDir, job.StreamID)
	if err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	defer release()
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	if err := rejectArchiveDirSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Result{}, PackageError{Phase: "input", Err: err}
	}
	if err := rejectArchiveDirSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	if _, err := safeRegularFileInfo(layout.FinalMKV()); err != nil {
		return Result{}, PackageError{Phase: "input", Err: err}
	}
	runner := m.Runner
	if runner == nil {
		runner = ffmpeg.CommandRunner{}
	}
	ffmpegBin := m.FFmpegBin
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	remuxOutput, cleanupRemuxOutput, err := createRemuxOutput(layout.RootDir, layout.FinalDir())
	if err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
	}
	defer cleanupRemuxOutput()
	if err := rejectExistingSymlink(layout.FinalMP4()); err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
	}
	remuxArgs := ffmpeg.BuildRemuxArgs(layout.FinalMKV(), remuxOutput)
	remuxStarted := time.Now()
	archiveSource := "final_mkv"
	archivePartial := false
	if err := runner.Run(ctx, ffmpegBin, remuxArgs); err != nil {
		// The live tee can leave a truncated Matroska header when an optional
		// provider/relay slave fails. If the independent preview completed,
		// recover the recording from its durable HLS playlist instead of losing
		// the archive and its Control Panel artifact report.
		if _, previewErr := safeRegularFileInfo(layout.PreviewPlaylist()); previewErr != nil {
			return Result{}, PackageError{Phase: "remux", Err: err}
		}
		fallbackArgs := ffmpeg.BuildRemuxArgs(layout.PreviewPlaylist(), remuxOutput)
		if fallbackErr := runner.Run(ctx, ffmpegBin, fallbackArgs); fallbackErr != nil {
			return Result{}, PackageError{Phase: "remux", Err: err}
		}
		archiveSource = "hls_preview_fallback"
		archivePartial = true
	}
	remuxDurationMS := time.Since(remuxStarted).Seconds() * 1000
	if _, err := safeRegularFileInfo(remuxOutput); err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
	}
	if err := replaceFileNoSymlink(remuxOutput, layout.FinalMP4()); err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
	}
	if _, err := safeRegularFileInfo(layout.TmpLogs()); err == nil {
		if err := copyFile(layout.TmpLogs(), layout.FinalLogs()); err != nil {
			return Result{}, PackageError{Phase: "package", Err: err}
		}
	} else if os.IsNotExist(err) {
		if err := writeFileNoSymlink(layout.FinalLogs(), []byte{}, 0o640); err != nil {
			return Result{}, PackageError{Phase: "package", Err: err}
		}
	} else if err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	if err := copyIfExists(layout.TmpCaptions(), layout.FinalCaptions()); err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	if err := copyIfExists(layout.TmpTranscript(), layout.FinalTranscript()); err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	startedAt := job.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	jst := time.FixedZone("JST", 9*60*60)
	extra := metadataExtra(job.DryRun, remuxDurationMS, job.ArchiveConfig)
	extra["archive_source"] = archiveSource
	extra["archive_partial"] = archivePartial
	extra["archive_run_id"] = job.ArchiveRunID
	metadata := Metadata{
		StreamID: job.StreamID, Name: job.Name, StartedAtJST: startedAt.In(jst).Format(time.RFC3339),
		Archive: ArchiveArtifactsForRun(job.StreamID, job.ArchiveRunID),
		Extra:   extra,
	}
	if dryRunner, ok := runner.(*ffmpeg.DryRunRunner); ok {
		metadata.Commands = RedactCommandsForLayout(layout, dryRunner.Commands)
	}
	files, err := collectArchiveFiles(layout, job.ArchiveConfig)
	if err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	uploader := m.uploaderForJob(job)
	if uploader == nil {
		uploader = m.Uploader
	}
	if uploader == nil {
		uploader = archive.DryRunUploader{}
	}
	upload, err := uploadArchiveFiles(ctx, uploader, job.Name, job.StreamID, startedAt.In(jst), files, layout.FinalMetadata(), &metadata)
	if err != nil {
		return Result{}, PackageError{Phase: "upload", Err: err}
	}
	metadata.Upload = upload
	if err := cleanupExpiredLocalArchives(m.ArchiveRoot, job.StreamID, job.ArchiveRunID, job.ArchiveConfig.RetentionDays, time.Now().UTC()); err != nil {
		return Result{}, PackageError{Phase: "retention", Err: err}
	}
	return Result{
		Layout:          layout,
		Metadata:        metadata,
		RemuxDurationMS: remuxDurationMS,
		ArchiveSource:   archiveSource,
		Partial:         archivePartial,
	}, nil
}

func archiveLayout(rootDir, streamID, archiveRunID string) (archive.Layout, error) {
	if archiveRunID == "" {
		return archive.NewLayout(rootDir, streamID)
	}
	return archive.NewRunLayout(rootDir, streamID, archiveRunID)
}

func acquirePackageLock(rootDir, streamID string) (func(), error) {
	key := filepath.Clean(rootDir) + string(os.PathSeparator) + streamID
	packageLocks.Lock()
	defer packageLocks.Unlock()
	if _, exists := packageLocks.active[key]; exists {
		return nil, ErrPackageInProgress
	}
	packageLocks.active[key] = struct{}{}
	return func() {
		packageLocks.Lock()
		delete(packageLocks.active, key)
		packageLocks.Unlock()
	}, nil
}
