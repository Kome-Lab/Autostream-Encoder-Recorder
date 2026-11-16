package lifecycleparticipant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

func fixture(t *testing.T) (OwnerSnapshot, lifecycle.Record) {
	t.Helper()
	var s OwnerSnapshot
	var r lifecycle.Record
	for name, target := range map[string]any{"owner.json": &s, "request.json": &r} {
		b, e := os.ReadFile(filepath.Join("testdata", name))
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(b, target); e != nil {
			t.Fatal(e)
		}
	}
	return s, r
}
func metadata(t *testing.T, r lifecycle.Record) []byte {
	t.Helper()
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func participantFixture(t *testing.T) (*Participant, SyntheticConfig, lifecycle.Record) {
	t.Helper()
	s, r := fixture(t)
	c := SyntheticConfig{Environment: "ISOLATED_TEST_ONLY", JournalDir: t.TempDir(), Current: s}
	p, e := NewSyntheticParticipant(c)
	if e != nil {
		t.Fatal(e)
	}
	return p, c, r
}
func pendingFacts(a Action) lifecycle.ObservationSet {
	obs := lifecycle.ObservationSet{Gate: lifecycle.Held, RemainingObligations: []lifecycle.ID{a.QueueWork.WorkID}}
	for _, sub := range []lifecycle.SubBoundary{lifecycle.RawWrite, lifecycle.Durability, lifecycle.QueueAdmission, lifecycle.QueueTerminal} {
		res := a.RawResource
		if sub == lifecycle.QueueAdmission || sub == lifecycle.QueueTerminal {
			res = a.QueueResource
		}
		value := lifecycle.SuccessAtBoundary
		if sub == lifecycle.QueueTerminal {
			value = lifecycle.Pending
		}
		o := lifecycle.BoundaryObservation{Address: lifecycle.BoundaryAddress{Kind: lifecycle.LocalPersistence, OwnerID: res.OwnerID, Resource: res, SubBoundary: sub}, Knowledge: lifecycle.Known, Presence: lifecycle.Present, Outcome: lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Observed, Value: &value, Observation: &a.Context}}
		switch sub {
		case lifecycle.RawWrite:
			offset, n := uint64(0), uint64(3)
			o.Details = &lifecycle.BoundaryDetails{RawExtent: &lifecycle.WriteExtent{ExtentRef: "fixture-local-write", Offset: &offset, Length: &n, RequestedLength: &n}}
		case lifecycle.Durability:
			o.Details = &lifecycle.BoundaryDetails{Durability: &lifecycle.DurabilityDetail{ProfileRef: a.Profile, Level: "SYNC_OBSERVED", WitnessRef: "fixture-local-sync-close"}}
		default:
			o.Details = &lifecycle.BoundaryDetails{Queue: &lifecycle.QueueDetail{Work: a.QueueWork, ReceiptRef: "fixture-queue-admission"}}
		}
		obs.Observations = append(obs.Observations, o)
	}
	obs.PhysicalObligations = []lifecycle.PhysicalObligation{{ObligationID: a.QueueWork.WorkID, Kind: lifecycle.QueueObligation, OwnerID: a.QueueResource.OwnerID, Resource: a.QueueResource, WorkRef: a.QueueWork.WorkID, Knowledge: lifecycle.KnowledgeUnknown, Terminal: lifecycle.Fact[lifecycle.ID]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: "NO_TERMINAL_CALLBACK"}}}}
	return obs
}
func testCutRestart(t *testing.T) {
	p, c, r := participantFixture(t)
	b := metadata(t, r)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan lifecycle.Result, 1)
	cut := make(chan lifecycle.Result, 1)
	var effects atomic.Int64
	go func() {
		_, check := p.Execute(context.Background(), "stream-1", b, func(_ context.Context, a Action) lifecycle.ObservationSet {
			effects.Add(1)
			close(entered)
			<-release
			return pendingFacts(a)
		})
		done <- check
	}()
	<-entered
	go func() { cut <- p.CloseAdmission(context.Background()) }()
	select {
	case <-cut:
		t.Fatal("cut passed an outstanding guarded append/child registration")
	default:
	}
	close(release)
	if check := <-done; check.Kind != lifecycle.Unknown {
		t.Fatalf("queue-release uncertainty lost: %+v", check)
	}
	if check := <-cut; check.Kind != lifecycle.StructurallyCoherent {
		t.Fatalf("cut: %+v", check)
	}
	record, _, check := p.CompletionSnapshot()
	if check.Kind != lifecycle.Unknown && check.Kind != lifecycle.PendingObligation {
		t.Fatalf("pending queue became completion: %+v %+v", record, check)
	}
	newer := clone(r)
	newer.Header.Correlation.AttemptID = "attempt-2"
	newer.Header.Correlation.ExecutionKey = "execution-2"
	if _, check = p.Execute(context.Background(), "stream-1", metadata(t, newer), func(context.Context, Action) lifecycle.ObservationSet {
		effects.Add(1)
		return lifecycle.ObservationSet{}
	}); check.Kind != lifecycle.RefusedUnchanged {
		t.Fatalf("closed admission: %+v", check)
	}
	if effects.Load() != 1 {
		t.Fatal("closed admission produced side effects")
	}
	c.Current = p.Snapshot()
	restarted, e := NewSyntheticParticipant(c)
	if e != nil {
		t.Fatal(e)
	}
	observed, check := restarted.Inspect("stream-1", b)
	if observed.Inspection == nil || check.Kind != lifecycle.Unknown || effects.Load() != 1 {
		t.Fatalf("restart replayed or erased uncertainty: %+v", check)
	}
	if _, check = restarted.Execute(context.Background(), "stream-1", b, func(context.Context, Action) lifecycle.ObservationSet {
		effects.Add(1)
		return lifecycle.ObservationSet{}
	}); check.Kind != lifecycle.Conflict {
		t.Fatalf("same attempt was rerun: %+v", check)
	}
	// A selective restore of the local closed marker cannot reopen against the
	// independent owner-supplied current admission model.
	if e = os.Remove(filepath.Join(c.JournalDir, "admission.closed.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = NewSyntheticParticipant(c); e == nil {
		t.Fatal("partial restored marker silently reopened admission")
	}
}
func TestIUWS3CurrentGenerationAndCallback(t *testing.T) {
	p, _, r := participantFixture(t)
	b := metadata(t, r)
	calls := 0
	accepted, check := p.Execute(context.Background(), "stream-1", b, func(_ context.Context, a Action) lifecycle.ObservationSet { calls++; return pendingFacts(a) })
	if accepted.Inspection == nil || check.Kind != lifecycle.Unknown || calls != 1 {
		t.Fatalf("positive registration: %+v", check)
	}
	other, err := NewSyntheticParticipant(SyntheticConfig{Environment: "ISOLATED_TEST_ONLY", JournalDir: p.dir, Current: p.Snapshot()})
	if err != nil {
		t.Fatal(err)
	}
	next := p.Snapshot()
	for i, e := range next.CurrentVector {
		if e.Key.Axis == lifecycle.RunAxis {
			x := lifecycle.ID("run-b")
			e.Value.Literal = &x
			e.Incarnation = "run-incarnation-b"
			next.CurrentVector[i] = e
		}
	}
	next.ClosureVector = clone(next.CurrentVector)
	next.OwnerRevision++
	if e := p.ReplaceOwnerSnapshot(next); e != nil {
		t.Fatal(e)
	}
	otherRequest := clone(r)
	otherRequest.Header.Correlation.AttemptID = "other-store-old-run"
	otherRequest.Header.Correlation.ExecutionKey = "other-store-old-execution"
	if _, c := other.Execute(context.Background(), "stream-1", metadata(t, otherRequest), func(context.Context, Action) lifecycle.ObservationSet { calls++; return lifecycle.ObservationSet{} }); c.Kind != lifecycle.Stale {
		t.Fatalf("independent cached owner revived old Run: %+v", c)
	}
	witness := lifecycle.ID("queue-consumed-synthetic")
	frame := lifecycle.ObservationContext{OwnerID: "encoder-1", FrameRef: "encoder-frame-2", Scope: "domain-1", SourceRevision: 2}
	terminal := lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &witness, Observation: &frame}
	if check = p.ObserveQueueTerminal("stream-1", b, 1, terminal); check.Kind != lifecycle.Stale {
		t.Fatalf("Run A callback mutated Run B: %+v", check)
	}
	// Returning the literal to A with a new incarnation must not revive A's callback.
	for i, e := range next.CurrentVector {
		if e.Key.Axis == lifecycle.RunAxis {
			x := lifecycle.ID("run-a")
			e.Value.Literal = &x
			next.CurrentVector[i] = e
		}
	}
	next.ClosureVector = clone(next.CurrentVector)
	next.OwnerRevision++
	if e := p.ReplaceOwnerSnapshot(next); e != nil {
		t.Fatal(e)
	}
	if check = p.ObserveQueueTerminal("stream-1", b, 1, terminal); check.Kind != lifecycle.Stale {
		t.Fatalf("run ABA revived callback: %+v", check)
	}
	wrong := clone(r)
	wrong.Header.ProducerRef.BootID = "boot-aba-other"
	if _, check = p.Execute(context.Background(), "stream-1", metadata(t, wrong), func(context.Context, Action) lifecycle.ObservationSet { calls++; return lifecycle.ObservationSet{} }); check.Kind != lifecycle.Conflict {
		t.Fatalf("old boot accepted: %+v", check)
	}
	missing := clone(r)
	missing.Vector.Vector = missing.Vector.Vector[1:]
	if _, check = p.Execute(context.Background(), "stream-1", metadata(t, missing), func(context.Context, Action) lifecycle.ObservationSet { calls++; return lifecycle.ObservationSet{} }); check.Kind == lifecycle.StructurallyCoherent {
		t.Fatal("missing axis accepted")
	}
	if calls != 1 {
		t.Fatal("stale/missing metadata invoked product callback")
	}
	old, check := p.Inspect("stream-1", b)
	if old.Inspection == nil || check.Kind != lifecycle.Unknown {
		t.Fatalf("original attempt not inspectable: %+v", check)
	}
}
func TestIUWS3QueueTerminalCAS(t *testing.T) {
	p, _, r := participantFixture(t)
	b := metadata(t, r)
	rec, _ := p.Execute(context.Background(), "stream-1", b, func(_ context.Context, a Action) lifecycle.ObservationSet { return pendingFacts(a) })
	if c := p.CloseAdmission(context.Background()); c.Kind != lifecycle.StructurallyCoherent {
		t.Fatal(c)
	}
	pending, _, pendingCheck := p.CompletionSnapshot()
	if pendingCheck.Kind != lifecycle.Unknown {
		t.Fatalf("closed queue unexpectedly terminal: %+v", pendingCheck)
	}
	cut := p.ClosureAdmission()
	original := p.Snapshot().Admission
	if cut.Header.RecordKey == original.Header.RecordKey || cut.Admission.Cut[0].Kind != "THROUGH" || *cut.Admission.Cut[0].LastSequence != 2 {
		t.Fatal("actual cut was not exported as a distinct immutable record")
	}
	witness := lifecycle.ID("fixture-owner-consumed-queue")
	frame := lifecycle.ObservationContext{OwnerID: "encoder-1", FrameRef: "encoder-frame-2", Scope: "domain-1", SourceRevision: 2}
	terminal := lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &witness, Observation: &frame}
	if c := p.ObserveQueueTerminal("stream-1", b, rec.Header.RecordRevision+1, terminal); c.Kind != lifecycle.Stale {
		t.Fatalf("CAS mismatch accepted: %+v", c)
	}
	if c := p.ObserveQueueTerminal("stream-1", b, rec.Header.RecordRevision, terminal); c.Kind != lifecycle.PendingObligation {
		t.Fatalf("terminal recorded with hold preserved: %+v", c)
	}
	if c := p.ObserveQueueTerminal("stream-1", b, rec.Header.RecordRevision, terminal); c.Kind != lifecycle.Stale {
		t.Fatalf("callback replay accepted: %+v", c)
	}
	completion, observations, c := p.CompletionSnapshot()
	if c.Kind != lifecycle.StructurallyCoherent || completion.Completion == nil {
		t.Fatalf("closed fixture local completion: %+v %+v", c, completion)
	}
	if observations.Gate != lifecycle.Held || p.ProductionQualified() {
		t.Fatal("completion claimed authority or released hold")
	}
	if completion.Header.RecordKey == pending.Header.RecordKey {
		t.Fatal("pending and terminal contents reused one immutable CC04 ID")
	}
	stable, _, _ := p.CompletionSnapshot()
	if stable.Header.RecordKey != completion.Header.RecordKey {
		t.Fatal("identical completion content has unstable ID")
	}
}
func TestIUWS3CancellationAndMalformedMetadata(t *testing.T) {
	p, _, r := participantFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if _, c := p.Execute(ctx, "stream-1", metadata(t, r), func(context.Context, Action) lifecycle.ObservationSet { calls++; return lifecycle.ObservationSet{} }); c.Kind != lifecycle.RefusedUnchanged {
		t.Fatal(c)
	}
	b := metadata(t, r)
	b = append(b[:len(b)-1], []byte(",\"Header\":{}}")...)
	if _, c := p.Execute(context.Background(), "stream-1", b, func(context.Context, Action) lifecycle.ObservationSet { calls++; return lifecycle.ObservationSet{} }); c.Kind == lifecycle.StructurallyCoherent {
		t.Fatal("duplicate metadata key accepted")
	}
	if calls != 0 {
		t.Fatal("refusal invoked callback")
	}
}
