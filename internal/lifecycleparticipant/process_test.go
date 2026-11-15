package lifecycleparticipant

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

func TestIUWS3IndependentProcess(t *testing.T) {
	if path := os.Getenv("IU_WS3_SYNTHETIC_PROCESS_CONFIG"); path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		var c SyntheticConfig
		if e = json.Unmarshal(b, &c); e != nil {
			t.Fatal(e)
		}
		p, e := NewSyntheticParticipant(c)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := os.ReadFile(path + ".request")
		if e != nil {
			t.Fatal(e)
		}
		r, check := p.Inspect("stream-1", raw)
		if r.Inspection == nil || check.Kind != lifecycle.Unknown {
			t.Fatalf("cross-process inspect: %+v", check)
		}
		var original lifecycle.Record
		if e = json.Unmarshal(raw, &original); e != nil {
			t.Fatal(e)
		}
		original.Header.Correlation.AttemptID = "child-attempt"
		original.Header.Correlation.ExecutionKey = "child-execution"
		if _, check = p.Execute(context.Background(), "stream-1", metadata(t, original), func(context.Context, Action) lifecycle.ObservationSet {
			t.Fatal("closed restart invoked product callback")
			return lifecycle.ObservationSet{}
		}); check.Kind != lifecycle.RefusedUnchanged {
			t.Fatalf("restart reopened admission: %+v", check)
		}
		t.Log("INSPECT_HELD_NO_REPLAY")
		return
	}
	p, c, r := participantFixture(t)
	b := metadata(t, r)
	if _, check := p.Execute(context.Background(), "stream-1", b, func(_ context.Context, a Action) lifecycle.ObservationSet { return pendingFacts(a) }); check.Kind != lifecycle.Unknown {
		t.Fatalf("queue uncertainty: %+v", check)
	}
	if check := p.CloseAdmission(context.Background()); check.Kind != lifecycle.StructurallyCoherent {
		t.Fatal(check)
	}
	c.Current = p.Snapshot()
	data, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path+".request", b, 0600); e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	command := exec.Command(executable, "-test.run=^TestIUWS3IndependentProcess$", "-test.v")
	command.Env = append(os.Environ(), "IU_WS3_SYNTHETIC_PROCESS_CONFIG="+path)
	output, e := command.CombinedOutput()
	if e != nil || !strings.Contains(string(output), "INSPECT_HELD_NO_REPLAY") {
		t.Fatalf("independent process: %v %s", e, output)
	}
}

func TestIUWS3ProcessSharedOwnerGuard(t *testing.T) {
	p, c, r := participantFixture(t)
	second, e := NewSyntheticParticipant(c)
	if e != nil {
		t.Fatal(e)
	}
	b := metadata(t, r)
	if _, check := p.Execute(context.Background(), "stream-1", b, func(_ context.Context, a Action) lifecycle.ObservationSet { return pendingFacts(a) }); check.Kind != lifecycle.Unknown {
		t.Fatal(check)
	}
	r.Header.Correlation.AttemptID = "second-process-attempt"
	r.Header.Correlation.ExecutionKey = "second-process-execution"
	calls := 0
	if _, check := second.Execute(context.Background(), "stream-1", metadata(t, r), func(context.Context, Action) lifecycle.ObservationSet { calls++; return lifecycle.ObservationSet{} }); check.Kind != lifecycle.Stale {
		t.Fatalf("different owner state admitted a duplicate sequence: %+v", check)
	}
	if calls != 0 {
		t.Fatal("stale process changed product state")
	}
	// Simulate an owner that crashed with the exclusive guard present. Startup
	// and timeouts must not remove it or turn held dispatch into a retry.
	lock := filepath.Join(c.JournalDir, "active-owner.lock")
	if e = os.WriteFile(lock, []byte("synthetic-crashed-holder"), 0600); e != nil {
		t.Fatal(e)
	}
	if check := p.CloseAdmission(context.Background()); check.Kind != lifecycle.Unknown {
		t.Fatalf("crashed holder was auto-released: %+v", check)
	}
	if _, e = os.Stat(lock); e != nil {
		t.Fatal("held guard was automatically removed")
	}
}
