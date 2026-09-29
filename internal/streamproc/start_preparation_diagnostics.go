package streamproc

import (
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"time"
)

func (m *Manager) reportPreparationExit(id StartPreparationIdentity, code string, job lifecycle.StreamJob, process RunningProcess, err error) {
	stderr := redactedProcessStderr(processStderr(process), job)
	attrs := map[string]any{"start_id": id.StartID, "job_generation": id.JobGeneration, "error_class": code, "stderr_tail_present": stderr != "", "stderr_tail": stderr}
	if value, ok := processExitCode(err); ok {
		attrs["exit_code"] = value
	}
	addProcessOutputDiagnostics(attrs, m.archiveRoot(), id.StreamID)
	signal := observability.Signal{Type: "error", Name: "encoder.start_preparation.exited", StreamID: id.StreamID, Status: "failed", Timestamp: time.Now().UTC(), Attributes: attrs}
	logProcessDiagnostic(signal)
	go m.report(signal)
}
