package streamproc

import (
	"context"
	"strconv"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

type ArtifactReporter interface {
	ReportArtifacts(ctx context.Context, streamID string, archiveRun control.ArchiveRun, artifacts []control.Artifact) error
}

type ArchivePackager interface {
	Package(ctx context.Context, job lifecycle.PackageJob) (lifecycle.Result, error)
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
					reportErr := m.ArtifactReporter.ReportArtifacts(reportCtx, job.StreamID, artifactReportArchiveRun(job), artifacts)
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
			err := m.ArtifactReporter.ReportArtifacts(reportCtx, job.StreamID, artifactReportArchiveRun(job), artifacts)
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
	return archive.NewRunLayout(root, job.StreamID, job.ArchiveRunID)
}

func artifactReportArchiveRun(job lifecycle.PackageJob) control.ArchiveRun {
	return control.ArchiveRun{ID: job.ArchiveRunID, StartedAt: job.StartedAt}
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
