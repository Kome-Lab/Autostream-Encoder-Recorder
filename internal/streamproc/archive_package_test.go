package streamproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestManagerStopAllAndDrainWaitsForArchivePackaging(t *testing.T) {
	packager := &fakePackager{root: t.TempDir(), delay: 80 * time.Millisecond}
	manager := &Manager{
		ArchiveRoot:   t.TempDir(),
		FFmpegBin:     "ffmpeg",
		Starter:       &fakeStarter{},
		Packager:      packager,
		InputResolver: testInputResolver, AllowHostnameInputs: true,
		OutputRelayMode: outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC)}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errs := manager.StopAllAndDrain(ctx); len(errs) != 0 {
		t.Fatalf("unexpected stop/drain errors: %#v", errs)
	}
	if !packager.calledWith(job.StreamID) {
		t.Fatalf("packager was not called before drain returned: %#v", packager.jobs)
	}
	status, err := manager.Status(job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "completed" {
		t.Fatalf("expected completed status after drain, got %#v", status)
	}
}

func TestManagerScrubsResolvedSecretsFromTrackedProcessAfterPackaging(t *testing.T) {
	packager := &fakePackager{root: t.TempDir()}
	manager := &Manager{
		ArchiveRoot:   t.TempDir(),
		FFmpegBin:     "ffmpeg",
		Starter:       &fakeStarter{},
		Packager:      packager,
		InputResolver: testInputResolver, AllowHostnameInputs: true,
		OutputRelayMode: outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", ArchiveRunID: "run-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera:camera-password@input.example.com/live",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
		StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: lifecycle.ArchiveConfig{
			AuthMode:                            "oauth2",
			ArchiveProfileID:                    "archive-profile-01",
			FolderID:                            "drive-folder-id",
			FolderIDSecretName:                  "drive_destination:dest-01:folder_id",
			ServiceAccountJSON:                  `{"type":"service_account","private_key":"raw-private-key"}`,
			ServiceAccountCredentialsSecretName: "google_drive_credentials",
			ClientSecret:                        "google-client-secret",
			ClientSecretSecretName:              "oauth_provider:provider-01:client_secret",
			RefreshToken:                        "google-refresh-token",
			RefreshTokenSecretName:              "oauth_account:account-01:refresh_token",
			SharedDrive:                         true,
		},
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errs := manager.StopAllAndDrain(ctx); len(errs) != 0 {
		t.Fatalf("unexpected stop/drain errors: %#v", errs)
	}
	if got, ok := packager.jobWith(job.StreamID); !ok || got.ArchiveConfig.FolderID != "drive-folder-id" || got.ArchiveConfig.RefreshToken != "google-refresh-token" {
		t.Fatalf("packager did not receive resolved archive secrets before scrub: %#v", got.ArchiveConfig)
	}
	manager.mu.Lock()
	tracked := manager.processes[job.StreamID]
	manager.mu.Unlock()
	if tracked == nil {
		t.Fatal("tracked process missing")
	}
	if tracked.job.StreamKey != "" || tracked.job.InputURL != "" || tracked.job.RTMPURL != "" {
		t.Fatalf("tracked job retained media secrets after packaging: %#v", tracked.job)
	}
	cfg := tracked.job.ArchiveConfig
	if cfg.FolderID != "" || cfg.ServiceAccountJSON != "" || cfg.ClientSecret != "" || cfg.RefreshToken != "" {
		t.Fatalf("tracked job retained archive secrets after packaging: %#v", cfg)
	}
	if cfg.FolderIDSecretName == "" || cfg.ClientSecretSecretName == "" || cfg.RefreshTokenSecretName == "" || !cfg.SharedDrive {
		t.Fatalf("tracked job should retain non-secret archive references: %#v", cfg)
	}
}

func TestManagerPackagesArchiveAfterStoppedProcess(t *testing.T) {
	reporter := &fakeReporter{}
	root := t.TempDir()
	packager := &fakePackager{root: root}
	artifactReporter := &fakeArtifactReporter{}
	manager := &Manager{
		ArchiveRoot:         filepath.Join(root, "archives"),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		ArtifactReporter:    artifactReporter,
		Packager:            packager,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: lifecycle.ArchiveConfig{
			AuthMode:    "service_account",
			FolderID:    "drive-folder-id",
			SharedDrive: true,
		},
	}
	snapshot, err := manager.Start(job)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Archive["final_artifact_set"]; got != "final/stream-01/run-01" {
		t.Fatalf("final artifact set = %q, want run-scoped path", got)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "completed" {
			if status.Archive["final_mp4"] != "final.mp4" {
				t.Fatalf("final_mp4 missing from completed snapshot: %#v", status)
			}
			if !packager.calledWith(job.StreamID) {
				t.Fatalf("packager was not called with stream id: %#v", packager.jobs)
			}
			if got, ok := packager.jobWith(job.StreamID); !ok || got.ArchiveConfig.FolderID != "drive-folder-id" || !got.ArchiveConfig.SharedDrive {
				t.Fatalf("archive config was not forwarded to packager: %#v", got.ArchiveConfig)
			}
			if got, _ := packager.jobWith(job.StreamID); got.ArchiveRunID != job.ArchiveRunID {
				t.Fatalf("archive run was not forwarded to packager: %#v", got)
			}
			if !reporter.has("archive.package.started") || !reporter.has("archive.package.completed") {
				t.Fatalf("missing package signals: %#v", reporter.names())
			}
			for _, metric := range []string{"archive.package_status", "archive.final_mp4_exists", "recorder.remux_duration_ms", "gdrive.upload_status", "gdrive.upload_retry_count", "gdrive.upload_duration_sec", "gdrive.upload_file_count", "gdrive.upload_folder_fingerprint_present", "gdrive.upload_final_mp4_fingerprint_present", "gdrive.upload_metadata_fingerprint_present"} {
				if !reporter.has(metric) {
					t.Fatalf("missing package metric %s: %#v", metric, reporter.names())
				}
			}
			if !reporter.hasValueAtLeast("archive.final_mp4_exists", 1) {
				t.Fatalf("run-scoped final mp4 was not reflected in metrics: %#v", reporter.signals)
			}
			if !artifactReporter.calledWith(job.StreamID, "final.mp4") {
				t.Fatalf("archive artifacts were not reported: %#v", artifactReporter.calls)
			}
			if !artifactReporter.calledWithRun(job.StreamID, job.ArchiveRunID) {
				t.Fatalf("archive run metadata was not reported: %#v", artifactReporter.calls)
			}
			if !reporter.has("archive.artifact_report.completed") {
				t.Fatalf("missing artifact report completion signal: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive was not packaged: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerArtifactReportFailureDoesNotFailCompletedArchive(t *testing.T) {
	reporter := &fakeReporter{}
	artifactReporter := &fakeArtifactReporter{err: errors.New("control panel unavailable")}
	manager := &Manager{
		ArchiveRoot:         t.TempDir(),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		ArtifactReporter:    artifactReporter,
		Packager:            &fakePackager{root: t.TempDir()},
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status(job.StreamID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "completed" {
			if !reporter.has("archive.artifact_report.failed") {
				t.Fatalf("missing artifact report failure signal: %#v", reporter.names())
			}
			event, ok := reporter.find("archive.artifact_report.failed")
			if !ok {
				t.Fatal("artifact report failure event was not recorded")
			}
			if _, leaked := event.Attributes["error"]; leaked {
				t.Fatalf("raw report error leaked: %#v", event.Attributes)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("artifact report failure changed archive completion: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerPackageFailureDoesNotReportGDriveUploadFailed(t *testing.T) {
	reporter := &fakeReporter{}
	artifactReporter := &fakeArtifactReporter{}
	archiveRoot := t.TempDir()
	layout, err := archive.NewLayout(archiveRoot, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMetadata(), []byte(`{"stream_id":"stream-01"}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	packager := &fakePackager{root: t.TempDir(), err: errors.New("remux failed")}
	manager := &Manager{ArchiveRoot: archiveRoot, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, ArtifactReporter: artifactReporter, Packager: packager, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if !reporter.has("archive.package_status") || !reporter.has("archive.package.failed") {
				t.Fatalf("missing package failure signals: %#v", reporter.names())
			}
			if reporter.has("gdrive.upload_status") {
				t.Fatalf("package failure must not be reported as gdrive upload failure: %#v", reporter.names())
			}
			if artifactReporter.callCount() != 0 {
				t.Fatalf("package failure without final.mp4 must not report artifacts: %#v", artifactReporter.calls)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive failure was not reported: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerPackageFailureReportsExistingLocalArtifactsWithoutChangingFailure(t *testing.T) {
	reporter := &fakeReporter{}
	artifactReporter := &fakeArtifactReporter{}
	root := t.TempDir()
	packageErr := lifecycle.PackageError{Phase: "upload", Err: errors.New("drive upload failed")}
	packager := &fakePackager{root: root, err: packageErr, artifactsBeforeError: true}
	manager := &Manager{
		ArchiveRoot:         filepath.Join(root, "archives"),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		ArtifactReporter:    artifactReporter,
		Packager:            packager,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status(job.StreamID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if status.Error != lifecycle.SafeErrorSummary(packageErr) {
				t.Fatalf("package failure was replaced: got %q want %q", status.Error, lifecycle.SafeErrorSummary(packageErr))
			}
			for _, name := range []string{"final.mp4", "metadata.json"} {
				if !artifactReporter.calledWith(job.StreamID, name) {
					t.Fatalf("existing local artifact %s was not reported: %#v", name, artifactReporter.calls)
				}
			}
			if !reporter.has("archive.artifact_report.completed") {
				t.Fatalf("missing artifact report completion signal: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive failure was not reported after local artifact reporting: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerPackageFailureArtifactReportFailurePreservesOriginalFailure(t *testing.T) {
	reporter := &fakeReporter{}
	artifactReporter := &fakeArtifactReporter{err: errors.New("control panel unavailable")}
	root := t.TempDir()
	packageErr := lifecycle.PackageError{Phase: "retention", Err: errors.New("retention failed")}
	manager := &Manager{
		ArchiveRoot:         filepath.Join(root, "archives"),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		ArtifactReporter:    artifactReporter,
		Packager:            &fakePackager{root: root, err: packageErr, artifactsBeforeError: true},
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status(job.StreamID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if status.Error != lifecycle.SafeErrorSummary(packageErr) {
				t.Fatalf("artifact report failure replaced package failure: got %q want %q", status.Error, lifecycle.SafeErrorSummary(packageErr))
			}
			if !artifactReporter.calledWith(job.StreamID, "final.mp4") {
				t.Fatalf("existing final.mp4 was not submitted before report failure: %#v", artifactReporter.calls)
			}
			if !reporter.has("archive.artifact_report.failed") {
				t.Fatalf("missing artifact report warning: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("package failure was not preserved after artifact report failure: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerPackageTimeoutStopsHungPackaging(t *testing.T) {
	reporter := &fakeReporter{}
	packager := &fakePackager{root: t.TempDir(), delay: time.Second}
	manager := &Manager{
		ArchiveRoot:         t.TempDir(),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		Packager:            packager,
		PackageTimeout:      20 * time.Millisecond,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if !reporter.has("archive.package.failed") {
				t.Fatalf("missing timeout failure signal: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("hung package operation was not timed out: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerUploadFailureReportsGDriveFailureWithoutRawError(t *testing.T) {
	reporter := &fakeReporter{}
	packager := &fakePackager{root: t.TempDir(), err: lifecycle.PackageError{Phase: "upload", Err: errors.New("https://example.com/upload?token=secret")}}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, Packager: packager, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key",
		ArchiveRunID: "run-01", StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if !reporter.hasValue("archive.package_status", 1) || !reporter.hasValue("gdrive.upload_status", 0) {
				t.Fatalf("expected upload failure metrics, got %#v", reporter.signalsSnapshot())
			}
			event, ok := reporter.find("archive.package.failed")
			if !ok {
				t.Fatalf("missing package failure event: %#v", reporter.names())
			}
			if _, leaked := event.Attributes["error"]; leaked || strings.Contains(anyMapString(event.Attributes), "secret") {
				t.Fatalf("raw failure detail leaked in event attributes: %#v", event.Attributes)
			}
			if event.Attributes["failure_phase"] != "upload" || event.Attributes["error_class"] != "archive_upload_failed" {
				t.Fatalf("unexpected failure attributes: %#v", event.Attributes)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive failure was not reported: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
