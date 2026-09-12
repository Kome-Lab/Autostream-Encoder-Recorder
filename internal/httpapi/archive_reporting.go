package httpapi

import (
	"context"
	"log"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
)

func reportPackageFailed(ctx context.Context, job lifecycle.PackageJob, elapsed time.Duration, err error) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	phase := lifecycle.ErrorPhase(err)
	if phase == "upload" {
		reportMetric(ctx, reporter, job.StreamID, "archive.package_status", 1)
		reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_status", 0)
	} else {
		reportMetric(ctx, reporter, job.StreamID, "archive.package_status", 0)
	}
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
	_ = reporter.Event(ctx, job.StreamID, "archive.package.failed", "failed", packageFailureAttributes(err, job.DryRun))
}

func reportPackageCompleted(ctx context.Context, job lifecycle.PackageJob, result lifecycle.Result, elapsed time.Duration) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	reportMetric(ctx, reporter, job.StreamID, "archive.package_status", 1)
	reportMetric(ctx, reporter, job.StreamID, "archive.package_partial", boolMetric(result.Partial))
	reportMetric(ctx, reporter, job.StreamID, "archive.final_mkv_usable", boolMetric(result.ArchiveSource == "final_mkv"))
	reportMetric(ctx, reporter, job.StreamID, "archive.final_mp4_exists", 1)
	reportMetric(ctx, reporter, job.StreamID, "recorder.remux_duration_ms", result.RemuxDurationMS)
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_status", 1)
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_retry_count", float64(maxInt(result.Metadata.Upload.Attempts-1, 0)))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_file_count", float64(result.Metadata.Upload.UploadedFileCount()))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_folder_fingerprint_present", boolMetric(result.Metadata.Upload.HasFolderFingerprint()))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_final_mp4_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("final.mp4")))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_metadata_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("metadata.json")))
	attributes := map[string]any{
		"dry_run":           job.DryRun,
		"upload_dry_run":    result.Metadata.Upload.DryRun,
		"upload_attempts":   result.Metadata.Upload.Attempts,
		"file_count":        len(result.Metadata.Upload.FileIDs),
		"remux_duration_ms": result.RemuxDurationMS,
		"archive_source":    result.ArchiveSource,
		"archive_partial":   result.Partial,
	}
	_ = reporter.Event(ctx, job.StreamID, "archive.package.completed", "completed", attributes)
	if result.Partial {
		_ = reporter.Event(ctx, job.StreamID, "archive.package.partial", "warning", map[string]any{
			"archive_source":  result.ArchiveSource,
			"archive_partial": true,
			"error_class":     "archive_source_fallback",
		})
	}
}

func reportControlPanelArtifacts(ctx context.Context, job lifecycle.PackageJob, result lifecycle.Result) {
	config := control.ConfigFromEnv()
	if config.ControlPanelURL == "" || config.Token == "" {
		return
	}
	artifacts := control.ArchiveArtifacts(result.Layout)
	if len(artifacts) == 0 {
		return
	}
	client := control.Client{Config: config}
	archiveRun := control.ArchiveRun{ID: job.ArchiveRunID, StartedAt: job.StartedAt}
	if err := client.ReportArtifacts(ctx, job.StreamID, archiveRun, artifacts); err != nil {
		log.Printf("control panel artifact report failed: %v", err)
	}
}

func reportMetric(ctx context.Context, reporter observability.Client, streamID, name string, value float64) {
	_ = reporter.Report(ctx, observability.Signal{Type: "metric", Name: name, StreamID: streamID, Value: &value})
}

func packageFailureAttributes(err error, dryRun bool) map[string]any {
	phase := lifecycle.ErrorPhase(err)
	if phase == "" {
		phase = "unknown"
	}
	return map[string]any{
		"failure_phase": phase,
		"error_class":   lifecycle.ErrorClass(err),
		"dry_run":       dryRun,
	}
}

func packageFailureResponse(err error, dryRun bool) map[string]any {
	attrs := packageFailureAttributes(err, dryRun)
	return map[string]any{
		"code":          "package_failed",
		"failure_phase": attrs["failure_phase"],
		"error_class":   attrs["error_class"],
		"dry_run":       attrs["dry_run"],
	}
}

func uploaderFromEnv(dryRun bool) archive.ArchiveUploader {
	return archive.DryRunUploader{}
}
