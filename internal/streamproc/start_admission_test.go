package streamproc

import (
	"context"
	"errors"
	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/manualhold"
	"os"
	"strconv"
	"testing"
	"time"
)

func msrTargetProducer(component lifecycle.ComponentID, serviceID string) lifecycle.ProducerRef {
	return lifecycle.ProducerRef{ComponentID: component, ServiceID: lifecycle.ID(serviceID), HostID: "synthetic-target", BootID: "synthetic-boot", ProcessIncarnation: lifecycle.ID("synthetic-process-" + strconv.Itoa(os.Getpid())), WriterID: "test-only-no-authority"}
}
func msrTargetRecord(t *testing.T, producer lifecycle.ProducerRef, streamID string, revision uint64, request, operation string) lifecycle.Record {
	t.Helper()
	resource := lifecycle.ResourceRef{Kind: lifecycle.Stream, OwnerID: producer.ServiceID, ResourceID: lifecycle.ID(streamID)}
	ref := lifecycle.ContractRef{ContractID: lifecycle.CC02, LogicalRevision: 1, SpecRef: lifecycle.SpecRef, ModuleCommitRef: manualhold.ModuleCommit}
	record := lifecycle.Record{Header: lifecycle.Header{ContractRef: ref, RecordKey: lifecycle.RecordKey{ContractRef: ref, OwnerID: "synthetic-cp", RecordID: lifecycle.ID(request)}, Correlation: lifecycle.Correlation{RequestID: lifecycle.ID(request), OperationID: lifecycle.ID(operation), AttemptID: lifecycle.ID("attempt-" + request), StepID: "start", ExecutionKey: lifecycle.ID("execution-" + request)}, DomainRef: lifecycle.DomainRef{DeploymentID: "synthetic-deployment", Realm: "synthetic-realm", DomainID: "synthetic-domain", MembershipRevision: 1, SnapshotRef: "membership-test", Resources: []lifecycle.ResourceRef{resource}}, ProducerRef: msrTargetProducer("C01", "synthetic-cp"), RecordRevision: revision, ProfileRef: lifecycle.ProfileRef{ProfileID: lifecycle.ExplicitResume, Revision: 1}}, Vector: &lifecycle.VectorRecord{}}
	for _, axis := range []lifecycle.Axis{lifecycle.Membership, lifecycle.Admission, lifecycle.Ownership, lifecycle.Fence, lifecycle.Boot, lifecycle.Process, lifecycle.RunAxis, lifecycle.ConfigAxis} {
		literal := lifecycle.ID("observed-" + string(axis))
		value := lifecycle.VersionedValue{Kind: lifecycle.PresentLiteral, Literal: &literal}
		if axis == lifecycle.Membership {
			v := uint64(1)
			value = lifecycle.VersionedValue{Kind: lifecycle.PresentUint64, Uint64: &v}
		}
		if axis == lifecycle.Boot {
			v := producer.BootID
			value.Literal = &v
		}
		if axis == lifecycle.Process {
			v := producer.ProcessIncarnation
			value.Literal = &v
		}
		record.Vector.Vector = append(record.Vector.Vector, lifecycle.RevisionEntry{Key: lifecycle.AxisKey{Axis: axis, OwnerID: resource.OwnerID, Resource: resource}, Incarnation: "synthetic-owner", Value: value, SourceFrameRef: "synthetic-cp-frame"})
	}
	if err := manualhold.Validate(record, streamID); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestMSREncoderPreparedCommitCannotCrossHold(t *testing.T) {
	starter := &fakeStarter{}
	manager := coverTestManager(t, starter, &coverTestFetcher{}, &coverTestWitness{})
	producer := msrTargetProducer("C04", "encoder-test")
	guard, err := manualhold.New(t.TempDir(), manualhold.InspectorFunc(func(_ context.Context, r lifecycle.Record) (lifecycle.Record, error) { return r, nil }), func() (lifecycle.ProducerRef, error) { return producer, nil })
	if err != nil {
		t.Fatal(err)
	}
	guard.Require = func() bool { return true }
	manager.StartAdmission = guard
	job := coverStartJob(nil)
	job.ArchiveRunID = "observed-RUN"
	job.StartedAt = time.Now().UTC()
	original := msrTargetRecord(t, producer, job.StreamID, 1, "prepare-start", "automatic-start")
	ctx, lease, err := guard.Enter(manualhold.WithRecord(t.Context(), original), job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !lease.Done() {
			_ = lease.Finish(manualhold.ErrUnknown)
		}
	}()
	identity := StartPreparationIdentity{StreamID: job.StreamID, StartID: "prepared-start-test", EncoderServiceID: "encoder-test", JobGeneration: job.VideoCoverStart.JobGeneration, ArchiveRunID: job.ArchiveRunID}
	preparation, ownedCtx, err := manager.BeginStartPreparation(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	state, err := manager.PrepareStart(ownedCtx, preparation, job)
	manager.FinishStartPreparation(preparation)
	if err != nil || state.Phase != "prepared" {
		t.Fatalf("prepare: %v %+v", err, state)
	}
	closed := msrTargetRecord(t, producer, job.StreamID, 2, "manual-stop", "manual-stop")
	if _, err = guard.Close(t.Context(), job.StreamID, closed); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.CommitStartPreparation(t.Context(), identity); !errors.Is(err, manualhold.ErrRefused) {
		t.Fatalf("old Commit crossed Hold: %v", err)
	}
	starter.mu.Lock()
	count := len(starter.processes)
	running := starter.process
	starter.mu.Unlock()
	if count != 1 || running.terminated || running.killed {
		t.Fatalf("Hold alone stopped existing preparation: starts=%d", count)
	}
	if _, err = manager.AbortStartPreparation(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
}
