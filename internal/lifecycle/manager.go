package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/redaction"
)

type Manager struct {
	ArchiveRoot    string
	FFmpegBin      string
	Runner         ffmpeg.Runner
	Uploader       archive.ArchiveUploader
	UploaderForJob func(PackageJob) archive.ArchiveUploader
	Profile        ffmpeg.EncoderProfile
}

type StreamJob struct {
	StreamID            string        `json:"stream_id"`
	Name                string        `json:"name"`
	InputURL            string        `json:"input_url"`
	InputMode           string        `json:"input_mode,omitempty"`
	RTMPURL             string        `json:"rtmp_url"`
	StreamKey           string        `json:"stream_key,omitempty"`
	StreamKeySecretName string        `json:"stream_key_secret_name,omitempty"`
	StartedAt           time.Time     `json:"started_at"`
	DryRun              bool          `json:"dry_run"`
	ArchiveConfig       ArchiveConfig `json:"archive_config,omitempty"`
}

type PackageJob struct {
	StreamID      string        `json:"stream_id"`
	Name          string        `json:"name"`
	StartedAt     time.Time     `json:"started_at"`
	DryRun        bool          `json:"dry_run"`
	ArchiveConfig ArchiveConfig `json:"archive_config,omitempty"`
}

type ArchiveConfig struct {
	DriveDestinationID                  string `json:"drive_destination_id,omitempty"`
	ArchiveProfileID                    string `json:"archive_profile_id,omitempty"`
	AuthMode                            string `json:"auth_mode,omitempty"`
	OAuthAccountID                      string `json:"oauth_account_id,omitempty"`
	OAuthProviderID                     string `json:"oauth_provider_id,omitempty"`
	FolderID                            string `json:"folder_id,omitempty"`
	FolderIDSecretName                  string `json:"folder_id_secret_name,omitempty"`
	ServiceAccountJSON                  string `json:"service_account_json,omitempty"`
	ServiceAccountSecretName            string `json:"service_account_json_secret_name,omitempty"`
	ServiceAccountCredentialsSecretName string `json:"service_account_credentials_secret_name,omitempty"`
	BasePath                            string `json:"base_path,omitempty"`
	SharedDrive                         bool   `json:"shared_drive,omitempty"`
	ClientID                            string `json:"client_id,omitempty"`
	ClientSecret                        string `json:"client_secret,omitempty"`
	ClientSecretSecretName              string `json:"client_secret_secret_name,omitempty"`
	RefreshToken                        string `json:"refresh_token,omitempty"`
	RefreshTokenSecretName              string `json:"refresh_token_secret_name,omitempty"`
}

type Metadata struct {
	StreamID     string               `json:"stream_id"`
	Name         string               `json:"name"`
	StartedAtJST string               `json:"started_at_jst"`
	Archive      map[string]string    `json:"archive"`
	Upload       archive.UploadResult `json:"upload"`
	Commands     []ffmpeg.Command     `json:"commands,omitempty"`
	Extra        map[string]any       `json:"extra,omitempty"`
}

type Result struct {
	Layout          archive.Layout `json:"layout"`
	Metadata        Metadata       `json:"metadata"`
	RemuxDurationMS float64        `json:"remux_duration_ms,omitempty"`
}

func ArchiveArtifacts(streamID string) map[string]string {
	return map[string]string{
		"tmp_artifact_set":   "tmp/" + streamID,
		"final_artifact_set": "final/" + streamID,
		"recording_mkv":      "final.mkv",
		"final_mp4":          "final.mp4",
		"metadata":           "metadata.json",
		"logs":               "logs.jsonl",
		"captions":           "captions.vtt",
		"transcript":         "transcript.json",
		"ffmpeg_progress":    "ffmpeg-progress.txt",
		"ffmpeg_audio":       "ffmpeg-audio-stats.txt",
	}
}

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

func (m Manager) DryRun(ctx context.Context, job StreamJob) (Result, error) {
	if job.StreamID == "" || job.Name == "" {
		return Result{}, errors.New("stream id and name are required")
	}
	if err := ffmpeg.ValidateInputTarget(job.InputURL); err != nil {
		return Result{}, err
	}
	if err := ffmpeg.ValidateOutputTarget(job.RTMPURL, job.StreamKey); err != nil {
		return Result{}, err
	}
	layout, err := archive.NewLayout(m.ArchiveRoot, job.StreamID)
	if err != nil {
		return Result{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Result{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return Result{}, err
	}
	runner := m.Runner
	if runner == nil {
		runner = &ffmpeg.DryRunRunner{}
	}
	profile := m.Profile
	if profile.Width == 0 {
		profile = ffmpeg.DefaultProfile()
	}
	ffmpegBin := m.FFmpegBin
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	liveArgs := BuildLiveArgs(job, layout.FinalMKV(), "", "", profile)
	if err := runner.Run(ctx, ffmpegBin, liveArgs); err != nil {
		return Result{}, err
	}
	remuxArgs := ffmpeg.BuildRemuxArgs(layout.FinalMKV(), layout.FinalMP4())
	remuxStarted := time.Now()
	if err := runner.Run(ctx, ffmpegBin, remuxArgs); err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
	}
	remuxDurationMS := time.Since(remuxStarted).Seconds() * 1000
	logLine := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "event": "dry_run.completed", "stream_id": job.StreamID}
	logJSON, _ := json.Marshal(logLine)
	if err := writeFileNoSymlink(layout.TmpLogs(), append(logJSON, '\n'), 0o640); err != nil {
		return Result{}, err
	}
	if err := copyFile(layout.TmpLogs(), layout.FinalLogs()); err != nil {
		return Result{}, err
	}
	uploader := m.uploaderForJob(PackageJob{
		StreamID:      job.StreamID,
		Name:          job.Name,
		StartedAt:     job.StartedAt,
		DryRun:        job.DryRun,
		ArchiveConfig: job.ArchiveConfig,
	})
	if uploader == nil {
		uploader = m.Uploader
	}
	if uploader == nil {
		uploader = archive.DryRunUploader{}
	}
	startedAt := job.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	jst := time.FixedZone("JST", 9*60*60)
	metadata := Metadata{
		StreamID: job.StreamID, Name: job.Name, StartedAtJST: startedAt.In(jst).Format(time.RFC3339),
		Archive: ArchiveArtifacts(job.StreamID),
		Extra:   metadataExtra(true, remuxDurationMS, job.ArchiveConfig),
	}
	if dryRunner, ok := runner.(*ffmpeg.DryRunRunner); ok {
		metadata.Commands = RedactCommandsForLayout(layout, dryRunner.Commands, job.StreamKey, job.InputURL, job.RTMPURL)
	}
	files := []archive.File{
		{LocalPath: layout.FinalMP4(), DrivePath: "final.mp4"},
		{LocalPath: layout.FinalLogs(), DrivePath: "logs.jsonl"},
	}
	upload, err := uploadArchiveFiles(ctx, uploader, job.Name, job.StreamID, startedAt.In(jst), files, layout.FinalMetadata(), &metadata)
	if err != nil {
		return Result{}, err
	}
	metadata.Upload = upload
	if err := writeJSON(layout.TmpMetadata(), metadata); err != nil {
		return Result{}, err
	}
	return Result{Layout: layout, Metadata: metadata, RemuxDurationMS: remuxDurationMS}, nil
}

func (m Manager) Package(ctx context.Context, job PackageJob) (Result, error) {
	if job.StreamID == "" || job.Name == "" {
		return Result{}, errors.New("stream id and name are required")
	}
	layout, err := archive.NewLayout(m.ArchiveRoot, job.StreamID)
	if err != nil {
		return Result{}, err
	}
	release, err := acquirePackageLock(layout.RootDir, job.StreamID)
	if err != nil {
		return Result{}, PackageError{Phase: "package", Err: err}
	}
	defer release()
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return Result{}, err
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
	if err := runner.Run(ctx, ffmpegBin, remuxArgs); err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
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
	metadata := Metadata{
		StreamID: job.StreamID, Name: job.Name, StartedAtJST: startedAt.In(jst).Format(time.RFC3339),
		Archive: ArchiveArtifacts(job.StreamID),
		Extra:   metadataExtra(job.DryRun, remuxDurationMS, job.ArchiveConfig),
	}
	if dryRunner, ok := runner.(*ffmpeg.DryRunRunner); ok {
		metadata.Commands = RedactCommandsForLayout(layout, dryRunner.Commands)
	}
	files, err := collectArchiveFiles(layout)
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
	return Result{Layout: layout, Metadata: metadata, RemuxDurationMS: remuxDurationMS}, nil
}

func (m Manager) uploaderForJob(job PackageJob) archive.ArchiveUploader {
	if m.UploaderForJob != nil {
		return m.UploaderForJob(job)
	}
	if job.DryRun || job.ArchiveConfig.AuthMode == "" {
		return nil
	}
	cfg := googleDriveConfigFromArchiveConfig(job.ArchiveConfig)
	return archive.RetryUploader{
		Inner:  archive.GoogleDriveAPIUploader{Config: cfg},
		Policy: archive.RetryPolicy{MaxAttempts: 5, BaseDelay: 2 * time.Second},
	}
}

func googleDriveConfigFromArchiveConfig(cfg ArchiveConfig) archive.GoogleDriveConfig {
	basePath := cfg.BasePath
	if strings.TrimSpace(basePath) == "" {
		basePath = "AutoStream"
	}
	return archive.GoogleDriveConfig{
		AuthMode:           cfg.AuthMode,
		ServiceAccountJSON: cfg.ServiceAccountJSON,
		FolderID:           cfg.FolderID,
		BasePath:           basePath,
		SharedDrive:        cfg.SharedDrive,
		ClientID:           cfg.ClientID,
		ClientSecret:       cfg.ClientSecret,
		RefreshToken:       cfg.RefreshToken,
	}
}

func metadataExtra(dryRun bool, remuxDurationMS float64, cfg ArchiveConfig) map[string]any {
	extra := map[string]any{"dry_run": dryRun, "remux_duration_ms": remuxDurationMS}
	if summary := archiveConfigMetadata(cfg); len(summary) > 0 {
		extra["archive_config"] = summary
	}
	return extra
}

func archiveConfigMetadata(cfg ArchiveConfig) map[string]any {
	out := map[string]any{}
	if cfg.DriveDestinationID != "" {
		out["drive_destination_id"] = cfg.DriveDestinationID
	}
	if cfg.ArchiveProfileID != "" {
		out["archive_profile_id"] = cfg.ArchiveProfileID
	}
	if cfg.AuthMode != "" {
		out["auth_mode"] = cfg.AuthMode
	}
	if cfg.OAuthAccountID != "" {
		out["oauth_account_id"] = cfg.OAuthAccountID
	}
	if cfg.OAuthProviderID != "" {
		out["oauth_provider_id"] = cfg.OAuthProviderID
	}
	if cfg.BasePath != "" {
		out["base_path"] = cfg.BasePath
	}
	if cfg.SharedDrive {
		out["shared_drive"] = true
	}
	if cfg.FolderID != "" || cfg.FolderIDSecretName != "" {
		out["folder_id_configured"] = true
	}
	if cfg.ServiceAccountJSON != "" || cfg.ServiceAccountSecretName != "" || cfg.ServiceAccountCredentialsSecretName != "" {
		out["service_account_json_configured"] = true
	}
	if cfg.ClientID != "" {
		out["client_id_configured"] = true
	}
	if cfg.ClientSecret != "" || cfg.ClientSecretSecretName != "" {
		out["client_secret_configured"] = true
	}
	if cfg.RefreshToken != "" || cfg.RefreshTokenSecretName != "" {
		out["refresh_token_configured"] = true
	}
	return out
}

func BuildLiveArgs(job StreamJob, archivePath, progressPath, audioStatsPath string, profile ffmpeg.EncoderProfile) []string {
	return BuildLiveArgsToOutputTarget(job, job.RTMPURL+"/"+job.StreamKey, archivePath, progressPath, audioStatsPath, profile)
}

func BuildLiveArgsToOutputTarget(job StreamJob, outputTarget, archivePath, progressPath, audioStatsPath string, profile ffmpeg.EncoderProfile) []string {
	if job.InputMode == "discord_opus_rtp" {
		return ffmpeg.BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetry(job.InputURL, outputTarget, archivePath, progressPath, audioStatsPath, profile)
	}
	if progressPath != "" || audioStatsPath != "" {
		return ffmpeg.BuildLiveArchiveArgsToOutputTargetWithTelemetry(job.InputURL, outputTarget, archivePath, progressPath, audioStatsPath, profile)
	}
	return ffmpeg.BuildLiveArchiveArgsToOutputTargetWithTelemetry(job.InputURL, outputTarget, archivePath, progressPath, audioStatsPath, profile)
}

func uploadArchiveFiles(ctx context.Context, uploader archive.ArchiveUploader, streamName, streamID string, startedAtJST time.Time, files []archive.File, metadataPath string, metadata *Metadata) (archive.UploadResult, error) {
	if uploader == nil {
		uploader = archive.DryRunUploader{}
	}
	metadataFile := archive.File{LocalPath: metadataPath, DrivePath: "metadata.json"}
	dataFiles := make([]archive.File, 0, len(files))
	for _, file := range files {
		if file.DrivePath == "metadata.json" {
			metadataFile = file
			continue
		}
		dataFiles = append(dataFiles, file)
	}

	upload := archive.UploadResult{FileIDs: map[string]string{}}
	if len(dataFiles) > 0 {
		var err error
		upload, err = uploader.Upload(ctx, streamName, streamID, startedAtJST, dataFiles)
		if err != nil {
			return archive.UploadResult{}, err
		}
	}
	metadata.Upload = upload
	if err := writeJSON(metadataPath, *metadata); err != nil {
		return archive.UploadResult{}, err
	}
	if info, err := safeRegularFileInfo(metadataFile.LocalPath); err == nil {
		metadataFile.SizeBytes = info.Size()
	}
	metadataUpload, err := uploader.Upload(ctx, streamName, streamID, startedAtJST, []archive.File{metadataFile})
	if err != nil {
		return archive.UploadResult{}, err
	}
	merged := mergeUploadResults(upload, metadataUpload)
	metadata.Upload = merged
	if err := writeJSON(metadataPath, *metadata); err != nil {
		return archive.UploadResult{}, err
	}
	return merged, nil
}

func mergeUploadResults(primary, metadata archive.UploadResult) archive.UploadResult {
	merged := primary
	if merged.FileIDs == nil {
		merged.FileIDs = map[string]string{}
	}
	if merged.FolderID == "" {
		merged.FolderID = metadata.FolderID
	}
	merged.DryRun = primary.DryRun || metadata.DryRun
	if metadata.Attempts > merged.Attempts {
		merged.Attempts = metadata.Attempts
	}
	for drivePath, fileID := range metadata.FileIDs {
		merged.FileIDs[drivePath] = fileID
	}
	return merged
}

func collectArchiveFiles(layout archive.Layout) ([]archive.File, error) {
	candidates := []struct {
		local string
		drive string
	}{
		{layout.FinalMP4(), "final.mp4"},
		{layout.FinalCaptions(), "captions.vtt"},
		{layout.FinalTranscript(), "transcript.json"},
		{layout.FinalMetadata(), "metadata.json"},
		{layout.FinalLogs(), "logs.jsonl"},
	}
	files := make([]archive.File, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := safeRegularFileInfo(candidate.local)
		if err != nil {
			if candidate.drive == "final.mp4" {
				return nil, err
			}
			continue
		}
		files = append(files, archive.File{LocalPath: candidate.local, DrivePath: candidate.drive, SizeBytes: info.Size()})
	}
	return files, nil
}

func copyIfExists(src, dst string) error {
	if _, err := safeRegularFileInfo(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return copyFile(src, dst)
}

func RedactCommandsForLayout(layout archive.Layout, commands []ffmpeg.Command, secrets ...string) []ffmpeg.Command {
	out := make([]ffmpeg.Command, 0, len(commands))
	for _, command := range commands {
		redacted := ffmpeg.Command{Bin: command.Bin, Args: redactArchivePaths(layout, redaction.Args(command.Args, secrets...))}
		out = append(out, redacted)
	}
	return out
}

func redactArchivePaths(layout archive.Layout, args []string) []string {
	replacements := []struct {
		from string
		to   string
	}{
		{layout.TmpFFmpegAudioStats(), "ffmpeg-audio-stats.txt"},
		{layout.TmpFFmpegProgress(), "ffmpeg-progress.txt"},
		{layout.TmpDiscordOpusSDP(), "discord-opus.sdp"},
		{layout.TmpDiscordOpus(), "discord-opus.jsonl"},
		{layout.FinalTranscript(), "transcript.json"},
		{layout.TmpTranscript(), "transcript.json"},
		{layout.FinalMetadata(), "metadata.json"},
		{layout.TmpMetadata(), "metadata.json"},
		{layout.FinalCaptions(), "captions.vtt"},
		{layout.TmpCaptions(), "captions.vtt"},
		{layout.FinalLogs(), "logs.jsonl"},
		{layout.TmpLogs(), "logs.jsonl"},
		{layout.FinalMKV(), "final.mkv"},
		{layout.FinalMP4(), "final.mp4"},
		{layout.FinalDir(), "final/" + layout.StreamID},
		{layout.TmpDir(), "tmp/" + layout.StreamID},
		{layout.RootDir, "<ARCHIVE_ROOT>"},
	}
	out := append([]string(nil), args...)
	for i := range out {
		for _, replacement := range replacements {
			out[i] = strings.ReplaceAll(out[i], replacement.from, replacement.to)
			out[i] = strings.ReplaceAll(out[i], filepath.ToSlash(replacement.from), replacement.to)
		}
	}
	return out
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileNoSymlink(path, append(body, '\n'), 0o640)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	body, err := readFileNoSymlink(src)
	if err != nil {
		return err
	}
	return writeFileNoSymlink(dst, body, 0o640)
}

func createRemuxOutput(rootDir, dir string) (string, func(), error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", func() {}, err
	}
	if err := rejectArchiveDirSymlinks(rootDir, dir); err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp(dir, ".final-remux-*.mp4")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := verifyOpenRegularFile(file, nil); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func replaceFileNoSymlink(src, dst string) error {
	if _, err := safeRegularFileInfo(src); err != nil {
		return err
	}
	if err := rejectExistingSymlink(dst); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if _, err := safeRegularFileInfo(dst); err != nil {
		return err
	}
	return nil
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

func safeRegularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive path must not be a symlink")
	}
	if info.IsDir() {
		return nil, errors.New("archive path must be a regular file")
	}
	return info, nil
}

func rejectExistingSymlink(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive path must not be a symlink")
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func rejectArchiveDirSymlinks(rootDir, dir string) error {
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
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive directory must not be a symlink")
	}
	if !rootInfo.IsDir() {
		return errors.New("archive root must be a directory")
	}
	if rel == "." {
		return nil
	}
	current := rootAbs
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive directory must not be a symlink")
		}
		if !info.IsDir() {
			return errors.New("archive path component must be a directory")
		}
	}
	return nil
}

func readFileNoSymlink(path string) ([]byte, error) {
	info, err := safeRegularFileInfo(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := verifyOpenRegularFile(file, info); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func writeFileNoSymlink(path string, body []byte, perm os.FileMode) error {
	return WriteFileNoSymlink(path, body, perm)
}

func WriteFileNoSymlink(path string, body []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	var file *os.File
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive path must not be a symlink")
		}
		if info.IsDir() {
			return errors.New("archive path must be a regular file")
		}
		file, err = os.OpenFile(path, os.O_WRONLY, perm)
		if err != nil {
			return err
		}
		if err := verifyOpenRegularFile(file, info); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := file.Seek(0, 0); err != nil {
			_ = file.Close()
			return err
		}
	} else if os.IsNotExist(err) {
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		if err := verifyOpenRegularFile(file, nil); err != nil {
			_ = file.Close()
			return err
		}
	} else {
		return err
	}
	defer file.Close()
	if err := file.Chmod(perm); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	return file.Sync()
}

func verifyOpenRegularFile(file *os.File, expected os.FileInfo) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("archive path must be a regular file")
	}
	if expected != nil && !os.SameFile(expected, info) {
		return errors.New("archive path changed while opening")
	}
	return nil
}
