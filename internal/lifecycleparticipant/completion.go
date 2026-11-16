package lifecycleparticipant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
)

type closedJournal struct {
	State     lifecycle.AdmissionModel
	Admission lifecycle.Record
}

// ObserveQueueTerminal is a trusted owner callback, not an HTTP operation. It
// requires the original generation, exact record CAS and a fresh owner frame.
// It records safety termination only; no remote result or mutation authority.
func (p *Participant) ObserveQueueTerminal(stream string, metadata []byte, expectedRevision uint64, terminal lifecycle.Fact[lifecycle.ID]) lifecycle.Result {
	if p == nil {
		return refusal(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	request, check := p.decode(metadata, stream)
	if check.Kind != lifecycle.StructurallyCoherent {
		return check
	}
	release, guardErr := p.acquireOwnerGuard()
	if guardErr != nil {
		return refusal(lifecycle.Unknown, "OWNER_GUARD_HELD_INSPECT_ONLY")
	}
	defer release()
	if p.checkOwnerState() != nil {
		return refusal(lifecycle.Stale, "OWNER_STATE_CHANGED_INSPECT_ONLY")
	}
	entry, readErr := p.readAttempt(key(request))
	if readErr != nil || lifecycle.CompareImmutable(entry.Request, request, p.current.IngressProfile).Kind != lifecycle.StructurallyCoherent {
		return refusal(lifecycle.Conflict, "CALLBACK_ATTEMPT_MISMATCH")
	}
	if entry.Reply.Header.RecordRevision != expectedRevision {
		return refusal(lifecycle.Stale, "CALLBACK_RECORD_CAS_MISMATCH")
	}
	if check = lifecycle.ValidateFact(terminal, p.current.IngressProfile.Limits); check.Kind != lifecycle.StructurallyCoherent {
		return check
	}
	if terminal.State != lifecycle.Observed || terminal.Observation.OwnerID != p.current.OwnerProducer.ServiceID || terminal.Observation.Scope != p.current.Frame.Scope || terminal.Observation.SourceRevision <= entry.Action.Context.SourceRevision {
		return refusal(lifecycle.Stale, "FRESH_OWNER_CALLBACK_REQUIRED")
	}
	obs := clone(entry.Reply.Inspection.Observations)
	admitted := false
	alreadyTerminal := false
	for _, o := range obs.Observations {
		if o.Address.SubBoundary == lifecycle.QueueAdmission && o.Knowledge == lifecycle.Known && o.Outcome.Value != nil && *o.Outcome.Value == lifecycle.SuccessAtBoundary {
			admitted = true
		}
		if o.Address.SubBoundary == lifecycle.QueueTerminal && o.Knowledge == lifecycle.Known && o.Outcome.Value != nil && *o.Outcome.Value != lifecycle.Pending {
			alreadyTerminal = true
		}
	}
	if !admitted || alreadyTerminal {
		return refusal(lifecycle.Conflict, "QUEUE_NOT_PENDING_ACCEPTED_WORK")
	}
	for i, o := range obs.Observations {
		if o.Address.SubBoundary == lifecycle.QueueTerminal {
			value := lifecycle.SuccessAtBoundary
			o.Knowledge = lifecycle.Known
			o.Presence = lifecycle.Present
			o.Outcome = lifecycle.Fact[lifecycle.Outcome]{State: lifecycle.Observed, Value: &value, Observation: terminal.Observation}
			o.Details = &lifecycle.BoundaryDetails{Queue: &lifecycle.QueueDetail{Work: entry.Action.QueueWork, ReceiptRef: *terminal.Value}}
			obs.Observations[i] = o
		}
	}
	for i, o := range obs.PhysicalObligations {
		if o.ObligationID == entry.Action.QueueWork.WorkID {
			o.Knowledge = lifecycle.Known
			o.Terminal = terminal
			obs.PhysicalObligations[i] = o
		}
	}
	remaining := []lifecycle.ID{}
	for _, id := range obs.RemainingObligations {
		if id != entry.Action.QueueWork.WorkID {
			remaining = append(remaining, id)
		}
	}
	obs.RemainingObligations = remaining
	next := entry
	next.Reply = p.reply(entry.Request, obs)
	next.Reply.Header.RecordRevision = expectedRevision + 1
	addFrames(&next.Reply)
	if e := p.nextResult(key(request), next); e != nil {
		return refusal(lifecycle.Unknown, "CALLBACK_COMMIT_UNKNOWN_INSPECT_ONLY")
	}
	p.entries[key(request)] = next
	return lifecycle.ValidateRecord(next.Reply, p.current.IngressProfile)
}
func addFrames(r *lifecycle.Record) {
	if r.Inspection == nil {
		return
	}
	seen := map[lifecycle.ID]bool{}
	for _, f := range r.Inspection.SourceFrames {
		seen[f.FrameRef] = true
	}
	add := func(o *lifecycle.ObservationContext) {
		if o != nil && !seen[o.FrameRef] {
			seen[o.FrameRef] = true
			r.Inspection.SourceFrames = append(r.Inspection.SourceFrames, lifecycle.SourceFrame{FrameRef: o.FrameRef, OwnerID: o.OwnerID, Scope: o.Scope, SourceRevision: o.SourceRevision})
		}
	}
	for _, o := range r.Inspection.Observations.Observations {
		add(o.Outcome.Observation)
	}
	for _, o := range r.Inspection.Observations.PhysicalObligations {
		add(o.Terminal.Observation)
	}
}

// CloseAdmission closes registration under the same private guard used by raw
// mutation. It does not stop a stream, drain a queue or issue a READY seal.
func (p *Participant) CloseAdmission(ctx context.Context) lifecycle.Result {
	if p == nil {
		return refusal(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return refusal(lifecycle.Conflict, "ADMISSION_ALREADY_CLOSED")
	}
	release, guardErr := p.acquireOwnerGuard()
	if guardErr != nil {
		return refusal(lifecycle.Unknown, "OWNER_GUARD_HELD_INSPECT_ONLY")
	}
	defer release()
	if p.checkOwnerState() != nil {
		return refusal(lifecycle.Stale, "OWNER_STATE_CHANGED_INSPECT_ONLY")
	}
	if ctx.Err() != nil {
		return refusal(lifecycle.PendingObligation, "ADMISSION_AND_HOLD_PRESERVED")
	}
	next := clone(p.admission)
	cut := []lifecycle.Frontier{}
	for _, ns := range next.AllowedNamespaces {
		last := uint64(0)
		for _, w := range next.Works {
			if w.Work.Namespace == ns && w.Work.Sequence > last {
				last = w.Work.Sequence
			}
		}
		f := lifecycle.Frontier{Kind: "EMPTY", Namespace: ns, EmptyFactRef: "explicit-empty-at-cut"}
		if last > 0 {
			f.Kind = "THROUGH"
			f.EmptyFactRef = ""
			f.LastSequence = &last
		}
		cut = append(cut, f)
	}
	var check lifecycle.Result
	next, check = lifecycle.ReduceAdmission(next, lifecycle.AdmissionEvent{Kind: lifecycle.BeginAdmissionCut, ExpectedRevision: next.Revision, Cut: cut}, p.current.IngressProfile.Limits)
	if check.Kind != lifecycle.StructurallyCoherent {
		return check
	}
	for _, guard := range next.RequiredGuardRefs {
		next, check = lifecycle.ReduceAdmission(next, lifecycle.AdmissionEvent{Kind: lifecycle.InstallAdmissionGuard, ExpectedRevision: next.Revision, GuardRef: guard, DurableFact: p.fact(guard)}, p.current.IngressProfile.Limits)
		if check.Kind != lifecycle.StructurallyCoherent {
			return check
		}
	}
	next, check = lifecycle.ReduceAdmission(next, lifecycle.AdmissionEvent{Kind: lifecycle.FinishAdmissionCut, ExpectedRevision: next.Revision}, p.current.IngressProfile.Limits)
	if check.Kind != lifecycle.StructurallyCoherent {
		return check
	}
	for _, ns := range next.AllowedNamespaces {
		closure := lifecycle.RegistrationClosure{MembershipSnapshot: next.MembershipSnapshot, AdmissionEpoch: next.AdmissionEpoch, Namespace: ns, RegistrationEpoch: "local-registration-closed", OwnerID: ns.OwnerID, DurableFactRef: "local-cut-guard-closed"}
		next, check = lifecycle.ReduceAdmission(next, lifecycle.AdmissionEvent{Kind: lifecycle.CloseLineageRegistration, ExpectedRevision: next.Revision, RegistrationClosure: closure}, p.current.IngressProfile.Limits)
		if check.Kind != lifecycle.StructurallyCoherent {
			return check
		}
	}
	cutRecord := clone(p.current.Admission)
	cutRecord.Admission.Cut = clone(next.Cut)
	cutBytes, _ := json.Marshal(cutRecord)
	cutDigest := sha256.Sum256(cutBytes)
	cutRecord.Header.RecordKey.RecordID = lifecycle.ID("cut-" + hex.EncodeToString(cutDigest[:]))
	cutRecord.Header.RecordRevision = 1
	if check = lifecycle.ValidateRecord(cutRecord, p.current.ClosureProfile); check.Kind != lifecycle.StructurallyCoherent {
		return check
	}
	if e := p.write("admission.closed.json", closedJournal{State: next, Admission: cutRecord}); e != nil {
		return refusal(lifecycle.Unknown, "CUT_COMMIT_UNKNOWN_HOLD_PRESERVED")
	}
	p.admission = next
	p.current.AdmissionState = clone(next)
	p.closed = true
	p.cutAdmission = &cutRecord
	return lifecycle.Result{Kind: lifecycle.StructurallyCoherent}
}

// ClosureAdmission is a distinct CC03 observation of the actual cut. It does not
// overwrite the original admission record or change accepted WorkRef bindings.
// Their relation is verified by this owner journal, not inferred from a public ID.
func (p *Participant) ClosureAdmission() lifecycle.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cutAdmission != nil {
		return clone(*p.cutAdmission)
	}
	return clone(p.current.Admission)
}
func (p *Participant) AdmissionSnapshot() lifecycle.AdmissionModel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return clone(p.admission)
}

// CompletionSnapshot exports a shared CC04 structural observation. It neither
// constructs CC05 nor interprets queue emptiness as proof. Unresolved raw/queue
// work stays UNKNOWN; all prior HELD receipt gates are preserved.
func (p *Participant) CompletionSnapshot() (lifecycle.Record, lifecycle.ObservationSet, lifecycle.Result) {
	if p == nil {
		return lifecycle.Record{}, lifecycle.ObservationSet{}, refusal(lifecycle.NeedsEvidence, "PARTICIPANT_REQUIRED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		return lifecycle.Record{}, lifecycle.ObservationSet{Gate: lifecycle.Held}, refusal(lifecycle.PendingObligation, "ADMISSION_NOT_CLOSED")
	}
	h := clone(p.current.Admission.Header)
	h.ContractRef.ContractID = lifecycle.CC04
	h.RecordKey = lifecycle.RecordKey{ContractRef: h.ContractRef, OwnerID: p.current.OwnerProducer.ServiceID, RecordID: "local-completion-snapshot"}
	h.ProducerRef = p.current.OwnerProducer
	h.ProfileRef = p.current.ClosureProfile.Ref
	c := lifecycle.CompletionRecord{ClosureID: p.current.Admission.Admission.ReservationRefs.DomainClosureRef, ReservationRefs: clone(p.current.Admission.Admission.ReservationRefs), SourceVector: clone(p.current.ClosureVector), LineageClosureFacts: clone(p.admission.ClosedRegistrations), TerminalPolicyRef: p.current.ClosureProfile.TerminalPolicyRef, ProofRefs: []lifecycle.ID{"local-registration-and-boundary-proof"}}
	obs := lifecycle.ObservationSet{Gate: lifecycle.Held}
	terminalWorks := map[lifecycle.ID]bool{}
	entryKeys := make([]string, 0, len(p.entries))
	for k := range p.entries {
		entryKeys = append(entryKeys, k)
	}
	sort.Strings(entryKeys)
	for _, entryKey := range entryKeys {
		e := p.entries[entryKey]
		if !reflect.DeepEqual(e.Request.Header.DomainRef, p.current.Membership.Header.DomainRef) {
			continue
		}
		terminal := len(e.Reply.Inspection.Observations.RemainingObligations) == 0
		for _, o := range e.Reply.Inspection.Observations.PhysicalObligations {
			if o.Knowledge != lifecycle.Known || o.Terminal.State != lifecycle.Observed {
				terminal = false
			}
			c.PhysicalObligations = append(c.PhysicalObligations, clone(o))
		}
		terminalWorks[e.Action.Work.WorkID] = terminal
		terminalWorks[e.Action.QueueWork.WorkID] = terminal
		// This projection is bounded local metadata only, never raw packet material.
		for _, o := range e.Reply.Inspection.Observations.Observations {
			v := clone(o)
			if v.Details != nil && v.Details.Durability != nil {
				v.Details.Durability.ProfileRef = p.current.ClosureProfile.Ref
			}
			// ObservationSet is current-address, not append history. Retain UNKNOWN
			// rather than allow a later success to erase unresolved prior work.
			found := false
			for i, old := range obs.Observations {
				if old.Address == v.Address {
					found = true
					if old.Knowledge == lifecycle.Known && v.Knowledge != lifecycle.Known {
						obs.Observations[i] = v
					}
					break
				}
			}
			if !found {
				obs.Observations = append(obs.Observations, v)
			}
		}
		obs.RemainingObligations = append(obs.RemainingObligations, e.Reply.Inspection.Observations.RemainingObligations...)
	}
	obs.PhysicalObligations = clone(c.PhysicalObligations)
	for _, base := range p.admission.Cut {
		last, terminal := uint64(0), uint64(0)
		for _, w := range p.admission.Works {
			if w.Work.Namespace == base.Namespace {
				if w.Work.Sequence > last {
					last = w.Work.Sequence
				}
				if w.Work.Sequence == terminal+1 && terminalWorks[w.Work.WorkID] {
					terminal = w.Work.Sequence
				}
			}
		}
		frontier := func(n uint64) lifecycle.Frontier {
			if n == 0 {
				return lifecycle.Frontier{Kind: "EMPTY", Namespace: base.Namespace, EmptyFactRef: "explicit-empty-frontier"}
			}
			return lifecycle.Frontier{Kind: "THROUGH", Namespace: base.Namespace, LastSequence: &n}
		}
		part := lifecycle.PartitionProof{Namespace: base.Namespace, CutFrontier: clone(base), FinalAcceptedFrontier: frontier(last), TerminalFrontier: frontier(terminal)}
		for _, cl := range p.admission.ClosedRegistrations {
			if cl.Namespace == base.Namespace {
				part.RegistrationClosed = cl
			}
		}
		if terminal < last {
			part.Holes = []lifecycle.SequenceRange{{First: terminal + 1, Last: last}}
		}
		c.Partitions = append(c.Partitions, part)
	}
	// This digest identifies immutable nonsecret structural metadata only. It is
	// neither a raw/secret commitment nor D4, a durable witness, or a READY seal.
	c.ProofRefs = nil
	metadataBytes, _ := json.Marshal(struct {
		Admission  lifecycle.RecordKey
		Completion lifecycle.CompletionRecord
		Boundaries lifecycle.ObservationSet
	}{p.current.Admission.Header.RecordKey, c, obs})
	metadataDigest := sha256.Sum256(metadataBytes)
	snapshotID := hex.EncodeToString(metadataDigest[:])
	h.RecordKey.RecordID = lifecycle.ID("completion-" + snapshotID)
	c.ProofRefs = []lifecycle.ID{lifecycle.ID("local-proof-" + snapshotID)}
	record := lifecycle.Record{Header: h, Completion: &c}
	return record, obs, lifecycle.ValidateRecord(record, p.current.ClosureProfile)
}

// MarshalMetadata encodes only the already-typed shared input. It adds no wire
// defaults, authority fields, raw payload, signatures or production qualification.
func MarshalMetadata(r lifecycle.Record) ([]byte, error) { return json.Marshal(r) }
