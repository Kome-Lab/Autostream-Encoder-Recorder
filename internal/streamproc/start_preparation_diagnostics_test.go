package streamproc

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"strings"
	"testing"
)

type preparationDiagnosticProcess struct{ preparationProcess }

func (*preparationDiagnosticProcess) Stderr() string {
	return "fixture input failure at rtmps://fixture.invalid/live2/private-start-key"
}

type preparationDiagnosticReporter struct{ signal chan observability.Signal }

func (r preparationDiagnosticReporter) Report(_ context.Context, s observability.Signal) error {
	r.signal <- s
	return nil
}
func TestStartPreparationExitDiagnosticKeepsIdentityAndRedactsSecret(t *testing.T) {
	reporter := preparationDiagnosticReporter{make(chan observability.Signal, 1)}
	m := &Manager{ArchiveRoot: t.TempDir(), Reporter: reporter}
	m.reportPreparationExit(StartPreparationIdentity{StreamID: "stream-diagnostic", StartID: "start-diagnostic", JobGeneration: 2}, "start_preparation_output_witness_failed", lifecycle.StreamJob{StreamKey: "private-start-key", RTMPURL: "rtmps://fixture.invalid/live2"}, &preparationDiagnosticProcess{}, errors.New("fixture exit"))
	var signal observability.Signal
	select {
	case signal = <-reporter.signal:
	case <-t.Context().Done():
		t.Fatal("diagnostic absent")
	}
	data, _ := json.Marshal(signal)
	if strings.Contains(string(data), "private-start-key") || strings.Contains(string(data), "/live2/private-start-key") {
		t.Fatal("diagnostic contains private output")
	}
	if signal.Attributes["start_id"] != "start-diagnostic" || signal.Attributes["job_generation"] != uint64(2) || signal.Attributes["stderr_tail_present"] != true {
		t.Fatal("diagnostic lost identity or original stderr presence")
	}
	if _, exists := signal.Attributes["pid"]; exists {
		t.Fatal("pid added to telemetry")
	}
}
