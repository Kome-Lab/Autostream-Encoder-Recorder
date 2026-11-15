// Package lifecycleparticipant owns an explicit isolated-test-only media participant.
// It issues no authority. Normal product startup does not construct this fixture.
package lifecycleparticipant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

const ModuleCommit lifecycle.ID = "d32ea3514b1c1010391caae0348b0ff9e751f447"

// OwnerSnapshot is injected by the owner, never supplied by an HTTP body as current.
// Profiles, bounds, generations and producer incarnations are explicit.
type OwnerSnapshot struct {
	OwnerRevision                  uint64
	IngressProfile, ClosureProfile lifecycle.ComparisonProfile
	Membership, Admission          lifecycle.Record
	CurrentVector, ClosureVector   lifecycle.RevisionVector
	OwnerProducer, IngressProducer lifecycle.ProducerRef
	StreamID                       string
	RawResource, QueueResource     lifecycle.ResourceRef
	Namespace                      lifecycle.Namespace
	Frame                          lifecycle.SourceFrame
	RawBudgetBytes                 uint64
	AdmissionState                 lifecycle.AdmissionModel
}
type SyntheticConfig struct {
	Environment string // exact ISOLATED_TEST_ONLY; no environment-variable fallback
	JournalDir  string
	Current     OwnerSnapshot
}

// Action is observation metadata, not a permit. A private ticket is consumed once
// while the participant holds its cut guard, before invoking the product callback.
type Action struct {
	Work, QueueWork            lifecycle.WorkRef
	Context                    lifecycle.ObservationContext
	Profile                    lifecycle.ProfileRef
	RawResource, QueueResource lifecycle.ResourceRef
	RawBudgetBytes             uint64
}
type Mutation func(context.Context, Action) lifecycle.ObservationSet
type ticket struct{ used bool }
type journalEntry struct {
	Request        lifecycle.Record
	Action         Action
	Reply          lifecycle.Record
	AdmissionState lifecycle.AdmissionModel
}
type Participant struct {
	mu           sync.Mutex
	current      OwnerSnapshot
	dir          string
	entries      map[string]journalEntry
	attempts     map[lifecycle.ID]string
	admission    lifecycle.AdmissionModel
	closed       bool
	cutAdmission *lifecycle.Record
}

func refusal(kind lifecycle.ResultKind, code string) lifecycle.Result {
	return lifecycle.Result{Kind: kind, Issues: []lifecycle.Issue{{Code: code, Path: "MediaParticipant"}}}
}
func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func snapshotValid(s OwnerSnapshot) error {
	if s.OwnerRevision == 0 {
		return errors.New("explicit owner generation revision required")
	}
	if s.StreamID == "" || s.RawBudgetBytes == 0 || s.RawResource.OwnerID != s.OwnerProducer.ServiceID || s.QueueResource.OwnerID != s.OwnerProducer.ServiceID || s.Frame.OwnerID != s.OwnerProducer.ServiceID || s.Frame.SourceRevision == 0 || s.Frame.FrameRef == "" || s.Frame.Scope == "" {
		return errors.New("explicit owner, stream, frame and byte bound required")
	}
	if s.IngressProfile.Ref.ProfileID != lifecycle.MediaIngress || s.ClosureProfile.Ref.ProfileID != lifecycle.ClosureAssess {
		return errors.New("explicit media and closure profiles required")
	}
	for _, r := range []lifecycle.Result{lifecycle.ValidateProfile(s.IngressProfile), lifecycle.ValidateProfile(s.ClosureProfile), lifecycle.ValidateRecord(s.Membership, s.ClosureProfile), lifecycle.ValidateRecord(s.Admission, s.ClosureProfile), lifecycle.ValidateVector(s.CurrentVector, s.IngressProfile), lifecycle.ValidateVector(s.ClosureVector, s.ClosureProfile)} {
		if r.Kind != lifecycle.StructurallyCoherent {
			return fmt.Errorf("owner input: %s %+v", r.Kind, r.Issues)
		}
	}
	if s.Membership.Membership == nil || s.Admission.Admission == nil || !reflect.DeepEqual(s.Membership.Header.DomainRef, s.Admission.Header.DomainRef) || s.Frame.Scope != s.Membership.Header.DomainRef.DomainID {
		return errors.New("membership/admission frame mismatch")
	}
	if s.RawResource.Kind != lifecycle.Run || string(s.RawResource.ResourceID) != s.StreamID || s.Namespace.Resource != s.QueueResource || s.Namespace.OwnerID != s.OwnerProducer.ServiceID || s.Namespace.ProducerIncarnation != s.OwnerProducer.ProcessIncarnation {
		return errors.New("stream/namespace owner mismatch")
	}
	if s.Membership.Header.ContractRef.ModuleCommitRef != ModuleCommit || s.Admission.Header.ContractRef.ModuleCommitRef != ModuleCommit || s.Admission.Header.RecordKey.OwnerID != s.OwnerProducer.ServiceID {
		return errors.New("shared pin/local admission owner mismatch")
	}
	owner, ingress, raw, queue, namespace := false, false, false, false, false
	for _, m := range s.Membership.Membership.Members {
		owner = owner || m.Resource == s.RawResource && m.Producer == s.OwnerProducer
		ingress = ingress || m.Resource == s.RawResource && m.Producer == s.IngressProducer
	}
	for _, r := range s.Membership.Header.DomainRef.Resources {
		raw = raw || r == s.RawResource
		queue = queue || r == s.QueueResource
	}
	for _, c := range s.Admission.Admission.Cut {
		namespace = namespace || c.Namespace == s.Namespace
	}
	if !owner || !ingress || !raw || !queue || !namespace {
		return errors.New("owner/member/resource coverage missing")
	}
	matched := map[lifecycle.Axis]bool{}
	for _, e := range s.CurrentVector {
		switch e.Key.Axis {
		case lifecycle.Membership:
			matched[e.Key.Axis] = e.Value.Uint64 != nil && *e.Value.Uint64 == s.Membership.Header.DomainRef.MembershipRevision
		case lifecycle.Admission:
			matched[e.Key.Axis] = e.Value.Literal != nil && *e.Value.Literal == s.Admission.Admission.AdmissionEpoch
		case lifecycle.Boot:
			matched[e.Key.Axis] = e.Value.Literal != nil && *e.Value.Literal == s.IngressProducer.BootID
		case lifecycle.Process:
			matched[e.Key.Axis] = e.Value.Literal != nil && *e.Value.Literal == s.IngressProducer.ProcessIncarnation
		}
	}
	for _, a := range []lifecycle.Axis{lifecycle.Membership, lifecycle.Admission, lifecycle.Boot, lifecycle.Process} {
		if !matched[a] {
			return errors.New("trusted vector binding mismatch")
		}
	}
	for _, current := range s.CurrentVector {
		for _, closure := range s.ClosureVector {
			if current.Key == closure.Key && (current.Incarnation != closure.Incarnation || !reflect.DeepEqual(current.Value, closure.Value)) {
				return errors.New("media/closure current vector overlap mismatch")
			}
		}
	}
	if s.AdmissionState.Key != s.Admission.Header.RecordKey || s.AdmissionState.MembershipSnapshot != s.Admission.Admission.MembershipSnapshot || s.AdmissionState.AdmissionEpoch != s.Admission.Admission.AdmissionEpoch {
		return errors.New("explicit current admission state required")
	}
	_, stateCheck := lifecycle.ReduceAdmission(s.AdmissionState, lifecycle.AdmissionEvent{Kind: lifecycle.AdmissionCancellation, ExpectedRevision: s.AdmissionState.Revision}, s.IngressProfile.Limits)
	if stateCheck.Kind != lifecycle.PendingObligation {
		return fmt.Errorf("invalid explicit admission state: %+v", stateCheck)
	}
	return nil
}
func NewSyntheticParticipant(c SyntheticConfig) (*Participant, error) {
	if c.Environment != "ISOLATED_TEST_ONLY" || !filepath.IsAbs(c.JournalDir) {
		return nil, errors.New("explicit isolated fixture and absolute journal directory required")
	}
	if err := snapshotValid(c.Current); err != nil {
		return nil, err
	}
	p := &Participant{current: clone(c.Current), dir: c.JournalDir, entries: map[string]journalEntry{}, attempts: map[lifecycle.ID]string{}}
	p.admission = clone(c.Current.AdmissionState)
	p.closed = p.admission.Lane != lifecycle.Open
	if err := os.MkdirAll(p.dir, 0700); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(p.dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		if err = p.write("owner.genesis.json", p.admission); err != nil {
			return nil, err
		}
		if err = p.saveOwnerGeneration(p.current); err != nil {
			return nil, err
		}
	} else if _, err = os.Stat(filepath.Join(p.dir, "owner.genesis.json")); err != nil {
		return nil, errors.New("journal genesis missing; restart remains held")
	}
	if err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}
func (*Participant) ProductionQualified() bool { return false }
func (p *Participant) Snapshot() OwnerSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return clone(p.current)
}
func key(r lifecycle.Record) string {
	b, _ := json.Marshal(r.Header.Correlation)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p *Participant) decode(raw []byte, stream string) (lifecycle.Record, lifecycle.Result) {
	r, c := lifecycle.DecodeRecord(raw, p.current.IngressProfile)
	if c.Kind != lifecycle.StructurallyCoherent {
		return r, c
	}
	if r.Vector == nil || r.Header.ContractRef.ModuleCommitRef != ModuleCommit || r.Header.ProducerRef != p.current.IngressProducer || r.Header.RecordKey.OwnerID != p.current.IngressProducer.ServiceID || stream != p.current.StreamID || !reflect.DeepEqual(r.Header.DomainRef, p.current.Membership.Header.DomainRef) {
		return r, refusal(lifecycle.Conflict, "OWNER_MEMBERSHIP_PRODUCER_MISMATCH")
	}
	return r, lifecycle.CompareVectors(r.Vector.Vector, p.current.CurrentVector, p.current.IngressProfile)
}
func (p *Participant) frame() lifecycle.ObservationContext {
	f := p.current.Frame
	return lifecycle.ObservationContext{OwnerID: f.OwnerID, FrameRef: f.FrameRef, Scope: f.Scope, SourceRevision: f.SourceRevision}
}
func (p *Participant) fact(id lifecycle.ID) lifecycle.Fact[lifecycle.ID] {
	o := p.frame()
	return lifecycle.Fact[lifecycle.ID]{State: lifecycle.Observed, Value: &id, Observation: &o}
}
func (p *Participant) reply(r lifecycle.Record, obs lifecycle.ObservationSet) lifecycle.Record {
	h := clone(r.Header)
	h.ContractRef.ContractID = lifecycle.CC08
	h.RecordKey = lifecycle.RecordKey{ContractRef: h.ContractRef, OwnerID: p.current.OwnerProducer.ServiceID, RecordID: lifecycle.ID("inspection-" + key(r))}
	h.ProducerRef = p.current.OwnerProducer
	q := lifecycle.QueryTarget{Correlation: r.Header.Correlation, BindingRef: r.Header.RecordKey.RecordID, ExecutionKey: r.Header.Correlation.ExecutionKey, TargetRecordRef: r.Header.RecordKey.RecordID, Scope: r.Header.DomainRef.DomainID, Purpose: lifecycle.PurposeLookup}
	obs.Gate = lifecycle.Held
	return lifecycle.Record{Header: h, Inspection: &lifecycle.InspectionRecord{Target: q, Observations: obs, ObservedScopes: []lifecycle.ID{q.Scope}, SourceFrames: []lifecycle.SourceFrame{p.current.Frame}, Coverage: p.fact("LOCAL_BOUNDARIES_ONLY")}}
}
func (p *Participant) initial(r lifecycle.Record, a Action) lifecycle.Record {
	obs := lifecycle.ObservationSet{Gate: lifecycle.Held, RemainingObligations: []lifecycle.ID{a.QueueWork.WorkID}}
	for _, sub := range []lifecycle.SubBoundary{lifecycle.RawWrite, lifecycle.Durability, lifecycle.QueueAdmission, lifecycle.QueueTerminal} {
		res := a.RawResource
		if sub == lifecycle.QueueAdmission || sub == lifecycle.QueueTerminal {
			res = a.QueueResource
		}
		obs.Observations = append(obs.Observations, lifecycle.BoundaryObservation{Address: lifecycle.BoundaryAddress{Kind: lifecycle.LocalPersistence, OwnerID: res.OwnerID, Resource: res, SubBoundary: sub}, Knowledge: lifecycle.KnowledgeUnknown, Presence: lifecycle.NotObserved, Outcome: lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Unavailable, Unavailable: &lifecycle.UnavailableReason{Code: "DISPATCH_POSSIBLE_INSPECT_ONLY"}}})
	}
	return p.reply(r, obs)
}
func (p *Participant) write(name string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(p.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	n, e := f.Write(b)
	if e == nil && n != len(b) {
		e = errors.New("short journal write")
	}
	if e == nil {
		e = f.Sync()
	}
	c := f.Close()
	if e == nil {
		e = c
	}
	return e
}
func (p *Participant) load() error {
	files, e := os.ReadDir(p.dir)
	if e != nil {
		return e
	}
	starts := []journalEntry{}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".start.json") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(p.dir, f.Name()))
		if e != nil {
			return e
		}
		var entry journalEntry
		if e = json.Unmarshal(b, &entry); e != nil {
			return e
		}
		k := key(entry.Request)
		if f.Name() != k+".start.json" || entry.Request.Vector == nil {
			return errors.New("invalid attempt journal")
		}
		if _, old := p.attempts[entry.Request.Header.Correlation.AttemptID]; old {
			return errors.New("attempt correlation collision")
		}
		p.entries[k] = entry
		p.attempts[entry.Request.Header.Correlation.AttemptID] = k
		starts = append(starts, entry)
		for rev := 1; ; rev++ {
			b, e = os.ReadFile(filepath.Join(p.dir, fmt.Sprintf("%s.result-%d.json", k, rev)))
			if os.IsNotExist(e) {
				break
			}
			if e != nil {
				return e
			}
			var updated journalEntry
			if e = json.Unmarshal(b, &updated); e != nil {
				return e
			}
			if !reflect.DeepEqual(updated.Request, entry.Request) || updated.Reply.Inspection == nil {
				return errors.New("immutable journal mismatch")
			}
			p.entries[k] = updated
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].AdmissionState.Revision < starts[j].AdmissionState.Revision })
	restored := p.admission
	if b, e := os.ReadFile(filepath.Join(p.dir, "owner.genesis.json")); e == nil {
		if e = json.Unmarshal(b, &restored); e != nil {
			return e
		}
	} else {
		return e
	}
	for _, entry := range starts {
		if entry.AdmissionState.Revision > restored.Revision {
			restored = entry.AdmissionState
		}
	}
	if b, e := os.ReadFile(filepath.Join(p.dir, "admission.closed.json")); e == nil {
		var closed closedJournal
		if e = json.Unmarshal(b, &closed); e != nil {
			return e
		}
		if lifecycle.ValidateRecord(closed.Admission, p.current.ClosureProfile).Kind != lifecycle.StructurallyCoherent || closed.Admission.Admission == nil || !reflect.DeepEqual(closed.Admission.Admission.Cut, closed.State.Cut) || closed.Admission.Header.RecordKey == p.current.Admission.Header.RecordKey {
			return errors.New("actual cut observation mismatch")
		}
		restored = closed.State
		p.cutAdmission = &closed.Admission
	}
	if !reflect.DeepEqual(restored, p.current.AdmissionState) {
		return errors.New("owner current admission differs from journal; restart remains held")
	}
	p.admission = restored
	p.closed = restored.Lane != lifecycle.Open
	return p.checkOwnerGeneration()
}
func (p *Participant) nextResult(k string, entry journalEntry) error {
	for rev := 1; ; rev++ {
		name := fmt.Sprintf("%s.result-%d.json", k, rev)
		_, e := os.Stat(filepath.Join(p.dir, name))
		if os.IsNotExist(e) {
			return p.write(name, entry)
		}
		if e != nil {
			return e
		}
	}
}

// Execute registers parent and queue child durably before any product mkdir,
// dedupe or write. A repeated POST is refused; inspect the original Attempt.
func (p *Participant) Execute(ctx context.Context, stream string, raw []byte, mutate Mutation) (lifecycle.Record, lifecycle.Result) {
	if p == nil {
		return lifecycle.Record{}, refusal(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, c := p.decode(raw, stream)
	if c.Kind != lifecycle.StructurallyCoherent {
		return lifecycle.Record{}, c
	}
	k := key(r)
	release, guardErr := p.acquireOwnerGuard()
	if guardErr != nil {
		return lifecycle.Record{}, refusal(lifecycle.Unknown, "OWNER_GUARD_HELD_INSPECT_ONLY")
	}
	defer release()
	if p.checkOwnerState() != nil {
		return lifecycle.Record{}, refusal(lifecycle.Stale, "OWNER_STATE_CHANGED_INSPECT_ONLY")
	}
	if old, ok := p.attempts[r.Header.Correlation.AttemptID]; ok {
		if old != k {
			return lifecycle.Record{}, refusal(lifecycle.Conflict, "ATTEMPT_TUPLE_REUSED")
		}
		if lifecycle.CompareImmutable(p.entries[k].Request, r, p.current.IngressProfile).Kind != lifecycle.StructurallyCoherent {
			return lifecycle.Record{}, refusal(lifecycle.Conflict, "IMMUTABLE_REQUEST_CHANGED")
		}
		return lifecycle.Record{}, refusal(lifecycle.Conflict, "SAME_ATTEMPT_INSPECT_REQUIRED")
	}
	if p.closed || p.admission.Lane != lifecycle.Open {
		return lifecycle.Record{}, refusal(lifecycle.RefusedUnchanged, "ADMISSION_CLOSED")
	}
	if ctx.Err() != nil {
		return lifecycle.Record{}, refusal(lifecycle.RefusedUnchanged, "CANCELLED_BEFORE_DISPATCH")
	}
	if mutate == nil {
		return lifecycle.Record{}, refusal(lifecycle.NeedsEvidence, "PRODUCT_MUTATION_REQUIRED")
	}
	next := p.admission
	last := uint64(0)
	for _, w := range next.Works {
		if w.Work.Namespace == p.current.Namespace && w.Work.Sequence > last {
			last = w.Work.Sequence
		}
	}
	if last > ^uint64(0)-2 {
		return lifecycle.Record{}, refusal(lifecycle.Conflict, "SEQUENCE_EXHAUSTED")
	}
	w := lifecycle.WorkRef{WorkID: lifecycle.ID("ingress-" + k), Namespace: p.current.Namespace, Sequence: last + 1, AdmissionRecordRef: p.current.Admission.Header.RecordKey.RecordID}
	child := lifecycle.WorkRef{WorkID: lifecycle.ID("queue-" + k), ParentWorkID: w.WorkID, Namespace: p.current.Namespace, Sequence: last + 2, AdmissionRecordRef: w.AdmissionRecordRef}
	for i, work := range []lifecycle.WorkRef{w, child} {
		kind := lifecycle.RegisterIndependentWork
		if i == 1 {
			kind = lifecycle.RegisterChildWork
		}
		next, c = lifecycle.ReduceAdmission(next, lifecycle.AdmissionEvent{Kind: kind, ExpectedRevision: next.Revision, Work: work, DurableFact: p.fact(work.WorkID)}, p.current.IngressProfile.Limits)
		if c.Kind != lifecycle.StructurallyCoherent {
			return lifecycle.Record{}, c
		}
	}
	a := Action{Work: w, QueueWork: child, Context: p.frame(), Profile: p.current.IngressProfile.Ref, RawResource: p.current.RawResource, QueueResource: p.current.QueueResource, RawBudgetBytes: p.current.RawBudgetBytes}
	entry := journalEntry{Request: r, Action: a, Reply: p.initial(r, a), AdmissionState: clone(next)}
	if e := p.write(k+".start.json", entry); e != nil {
		return lifecycle.Record{}, refusal(lifecycle.Unknown, "REGISTRATION_COMMIT_UNKNOWN_INSPECT_ONLY")
	}
	p.admission = next
	p.entries[k] = entry
	p.attempts[r.Header.Correlation.AttemptID] = k
	p.current.AdmissionState = clone(next)
	private := ticket{}
	if private.used {
		panic("consumed private participant")
	}
	private.used = true
	obs := mutate(ctx, clone(a))
	for _, o := range obs.Observations {
		if o.Address.Kind != lifecycle.LocalPersistence || o.Address.OwnerID != p.current.OwnerProducer.ServiceID || (o.Address.Resource != a.RawResource && o.Address.Resource != a.QueueResource) {
			return entry.Reply, refusal(lifecycle.Unknown, "LOCAL_OBSERVATION_SCOPE_MISMATCH")
		}
	}
	entry.Reply = p.reply(r, obs)
	if e := p.nextResult(k, entry); e != nil {
		return p.entries[k].Reply, refusal(lifecycle.Unknown, "RESULT_COMMIT_UNKNOWN_INSPECT_ONLY")
	}
	p.entries[k] = entry
	return clone(entry.Reply), lifecycle.ValidateRecord(entry.Reply, p.current.IngressProfile)
}

// Inspect returns the same immutable Attempt without reappend, requeue or new IDs.
func (p *Participant) Inspect(stream string, raw []byte) (lifecycle.Record, lifecycle.Result) {
	if p == nil {
		return lifecycle.Record{}, refusal(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, c := lifecycle.DecodeRecord(raw, p.current.IngressProfile)
	if c.Kind != lifecycle.StructurallyCoherent || r.Vector == nil {
		return lifecycle.Record{}, c
	}
	entry, readErr := p.readAttempt(key(r))
	if readErr != nil || stream != p.current.StreamID {
		return lifecycle.Record{}, refusal(lifecycle.Unknown, "ATTEMPT_NOT_OBSERVED")
	}
	if lifecycle.CompareImmutable(entry.Request, r, p.current.IngressProfile).Kind != lifecycle.StructurallyCoherent {
		return lifecycle.Record{}, refusal(lifecycle.Conflict, "INSPECT_BINDING_MISMATCH")
	}
	return clone(entry.Reply), lifecycle.ValidateRecord(entry.Reply, p.current.IngressProfile)
}

// ReplaceOwnerSnapshot is trusted owner DI. Old receipts stay inspectable; delayed
// callbacks must still match their original vector before adding observations.
func (p *Participant) ReplaceOwnerSnapshot(next OwnerSnapshot) error {
	if e := snapshotValid(next); e != nil {
		return e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	release, guardErr := p.acquireOwnerGuard()
	if guardErr != nil {
		return guardErr
	}
	defer release()
	if err := p.checkOwnerState(); err != nil {
		return err
	}
	if p.current.OwnerRevision == ^uint64(0) || next.OwnerRevision != p.current.OwnerRevision+1 {
		return errors.New("explicit owner generation CAS revision required")
	}
	if !reflect.DeepEqual(next.IngressProfile, p.current.IngressProfile) || next.StreamID != p.current.StreamID {
		return errors.New("new profile/stream requires a new participant")
	}
	if lifecycle.CompareImmutable(next.Membership, p.current.Membership, p.current.ClosureProfile).Kind != lifecycle.StructurallyCoherent || lifecycle.CompareImmutable(next.Admission, p.current.Admission, p.current.ClosureProfile).Kind != lifecycle.StructurallyCoherent {
		return errors.New("membership/admission change requires a distinct explicit participant/journal")
	}
	if next.Admission.Header.RecordKey != p.current.Admission.Header.RecordKey || !reflect.DeepEqual(next.AdmissionState, p.admission) {
		return errors.New("admission rotation requires a distinct explicit participant/journal")
	}
	if err := p.saveOwnerGeneration(next); err != nil {
		return err
	}
	p.current = clone(next)
	return nil
}
