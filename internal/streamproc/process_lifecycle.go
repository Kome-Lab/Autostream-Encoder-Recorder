package streamproc

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
)

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
