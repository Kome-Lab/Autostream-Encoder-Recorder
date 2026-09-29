package streamproc

import (
	"context"
	"errors"
	"fmt"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type preparationProcess struct {
	exited chan struct{}
	once   sync.Once
	kills  atomic.Int32
	waits  atomic.Int32
}

func (p *preparationProcess) PID() int    { return 1234 }
func (p *preparationProcess) Wait() error { p.waits.Add(1); <-p.exited; return nil }
func (p *preparationProcess) Kill() error {
	p.kills.Add(1)
	p.once.Do(func() { close(p.exited) })
	return nil
}
func (p *preparationProcess) Terminate() error { return p.Kill() }

type preparationStarter struct {
	mu        sync.Mutex
	processes []*preparationProcess
}

func (s *preparationStarter) Start(context.Context, string, []string) (RunningProcess, error) {
	p := &preparationProcess{exited: make(chan struct{})}
	s.mu.Lock()
	s.processes = append(s.processes, p)
	s.mu.Unlock()
	return p, nil
}

type preparationWitness struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (w *preparationWitness) Apply(ctx context.Context, _ *imagefeed.Source, _ []byte, initial bool, _ string) error {
	w.calls.Add(1)
	if !initial {
		return errors.New("not initial")
	}
	if w.entered != nil {
		close(w.entered)
	}
	if w.release != nil {
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if w.fail {
		return errors.New("fixture witness failure")
	}
	return ctx.Err()
}
func preparationManager(t *testing.T, w *preparationWitness) (*Manager, *preparationStarter, lifecycle.StreamJob, StartPreparationIdentity) {
	t.Helper()
	m := coverTestManager(t, &fakeStarter{}, &coverTestFetcher{}, &coverTestWitness{})
	starter := &preparationStarter{}
	m.Starter = starter
	m.CoverWitness = w
	job := coverStartJob(nil)
	job.ArchiveRunID = "archive-7"
	job.StartedAt = time.Now().UTC()
	id := StartPreparationIdentity{StreamID: job.StreamID, StartID: "attempt-7", EncoderServiceID: "encoder-01", JobGeneration: 7, ArchiveRunID: job.ArchiveRunID}
	return m, starter, job, id
}
func prepareFixture(t *testing.T, m *Manager, job lifecycle.StreamJob, id StartPreparationIdentity) *StartPreparation {
	t.Helper()
	p, ctx, err := m.BeginStartPreparation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.PrepareStart(ctx, p, job)
	m.FinishStartPreparation(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.AbortStartPreparation(context.Background(), id) })
	return p
}
func waitPreparationChannel(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded lifecycle wait exceeded")
	}
}
func TestStartPreparationDoesNotWitnessOrPublishRunning(t *testing.T) {
	w := &preparationWitness{}
	m, starter, job, id := preparationManager(t, w)
	p := prepareFixture(t, m, job, id)
	state, err := m.StartPreparationStatus(id.StreamID, id.StartID)
	if err != nil || state.Phase != "prepared" || state.Process != nil || state.CoverState != nil || w.calls.Load() != 0 {
		t.Fatalf("prepared became final: %+v %v", state, err)
	}
	snapshot, _ := m.Status(id.StreamID)
	if snapshot.Status != "starting" || snapshot.PID == 0 {
		t.Fatalf("waiting process missing: %+v", snapshot)
	}
	if _, err := m.VideoCoverState(id.StreamID); err == nil {
		t.Fatal("prepared cover marked applied")
	}
	if _, _, err := m.BeginStartPreparation(context.Background(), id); !errors.Is(err, ErrPreparationConflict) {
		t.Fatal("duplicate prepare accepted")
	}
	other := job
	other.StreamID = "another-stream"
	if _, err := m.Start(other); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("legacy escaped slot guard")
	}
	if len(starter.processes) != 1 {
		t.Fatal("duplicate spawn")
	}
	if _, err := m.AbortStartPreparation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitPreparationChannel(t, p.cleanupDone)
	if starter.processes[0].waits.Load() != 1 {
		t.Fatal("process was not reaped exactly once")
	}
}
func TestStartPreparationCommitCASAndContextTransfer(t *testing.T) {
	w := &preparationWitness{entered: make(chan struct{}), release: make(chan struct{})}
	m, starter, job, id := preparationManager(t, w)
	p := prepareFixture(t, m, job, id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.CommitStartPreparation(ctx, id); done <- err }()
	waitPreparationChannel(t, w.entered)
	m.mu.Lock()
	p.expiresAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	m.expirePreparation(p)
	if _, err := m.CommitStartPreparation(context.Background(), id); !errors.Is(err, ErrPreparationConflict) {
		t.Fatal("concurrent commit was accepted")
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cancel()
	state, err := m.CommitStartPreparation(context.Background(), id)
	if err != nil || state.Phase != "running" || state.CoverState == nil || state.CoverState.AppliedWitness == nil {
		t.Fatalf("commit not witnessed: %+v %v", state, err)
	}
	if w.calls.Load() != 1 || len(starter.processes) != 1 || starter.processes[0].kills.Load() != 0 {
		t.Fatal("repeat witness/spawn or request cancellation killed runtime")
	}
}
func TestStartPreparationOldAbortExpiryCannotStopReplacement(t *testing.T) {
	m, starter, job, id := preparationManager(t, &preparationWitness{})
	old := prepareFixture(t, m, job, id)
	if _, err := m.AbortStartPreparation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	job.VideoCoverStart.JobGeneration = 8
	job.ArchiveRunID = "archive-8"
	next := id
	next.StartID = "attempt-8"
	next.JobGeneration = 8
	next.ArchiveRunID = job.ArchiveRunID
	prepareFixture(t, m, job, next)
	if _, err := m.AbortStartPreparation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	m.expirePreparation(old)
	wrong := next
	wrong.JobGeneration = 7
	if _, err := m.AbortStartPreparation(context.Background(), wrong); !errors.Is(err, ErrPreparationConflict) {
		t.Fatal("stale generation accepted")
	}
	// A delayed exit observer is also fenced by the captured process pointer.
	m.wait(id.StreamID, starter.processes[0], nil)
	state, _ := m.StartPreparationStatus(next.StreamID, next.StartID)
	if state.Phase != "prepared" || starter.processes[1].kills.Load() != 0 {
		t.Fatal("old callback touched replacement")
	}
}
func TestStartPreparationFailureExpiryAndUnknownRestart(t *testing.T) {
	for _, cause := range []string{"exit", "expire", "witness", "cancel"} {
		t.Run(cause, func(t *testing.T) {
			w := &preparationWitness{fail: cause == "witness"}
			m, starter, job, id := preparationManager(t, w)
			p := prepareFixture(t, m, job, id)
			switch cause {
			case "exit":
				_ = starter.processes[0].Kill()
				waitPreparationChannel(t, p.cleanupDone)
			case "expire":
				m.mu.Lock()
				p.expiresAt = time.Now().Add(-time.Second)
				m.mu.Unlock()
				m.expirePreparation(p)
			case "witness":
				if _, err := m.CommitStartPreparation(context.Background(), id); err == nil {
					t.Fatal("failed witness succeeded")
				}
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := m.CommitStartPreparation(ctx, id); err == nil {
					t.Fatal("canceled commit succeeded")
				}
			}
			state, _ := m.StartPreparationStatus(id.StreamID, id.StartID)
			if state.Phase != "failed" && state.Phase != "expired" {
				t.Fatalf("phase=%s", state.Phase)
			}
			m2 := &Manager{ArchiveRoot: m.ArchiveRoot}
			if _, err := m2.StartPreparationStatus(id.StreamID, id.StartID); !errors.Is(err, ErrPreparationUnknown) {
				t.Fatal("restart inferred receipt")
			}
			if _, _, err := m2.BeginStartPreparation(context.Background(), id); !errors.Is(err, ErrPreparationConflict) {
				t.Fatal("restart reused high-water epoch")
			}
		})
	}
}
func TestStartPreparationSingleReservationRaceAndBoundedReceipts(t *testing.T) {
	m, _, _, id := preparationManager(t, &preparationWitness{})
	ready := make(chan struct{})
	owners := make(chan *StartPreparation, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready
			next := id
			next.StartID = fmt.Sprintf("race-%d", i)
			p, _, err := m.BeginStartPreparation(context.Background(), next)
			if err == nil {
				owners <- p
			}
		}(i)
	}
	close(ready)
	wg.Wait()
	close(owners)
	count := 0
	for p := range owners {
		count++
		m.FinishStartPreparation(p)
		if err := m.FailStartPreparation(p); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("slot owners=%d", count)
	}
	for i := 0; i < 66; i++ {
		next := id
		next.StartID = fmt.Sprintf("receipt-%d", i)
		p, _, err := m.BeginStartPreparation(context.Background(), next)
		if err != nil {
			t.Fatal(err)
		}
		m.FinishStartPreparation(p)
		if err := m.FailStartPreparation(p); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.preparations) != 64 {
		t.Fatalf("receipt count=%d", len(m.preparations))
	}
	if _, err := m.StartPreparationStatus(id.StreamID, "receipt-0"); !errors.Is(err, ErrPreparationUnknown) {
		t.Fatal("evicted receipt became positive")
	}
}

type preparationBlockingStarter struct {
	entered, release chan struct{}
	delegate         preparationStarter
}

func (s *preparationBlockingStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	close(s.entered)
	<-s.release
	return s.delegate.Start(ctx, bin, args)
}
func TestStartPreparationCancelDuringSpawnReapsBeforeReuse(t *testing.T) {
	m, _, job, id := preparationManager(t, &preparationWitness{})
	starter := &preparationBlockingStarter{entered: make(chan struct{}), release: make(chan struct{})}
	m.Starter = starter
	p, owned, err := m.BeginStartPreparation(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan error, 1)
	go func() { _, e := m.PrepareStart(owned, p, job); m.FinishStartPreparation(p); prepared <- e }()
	waitPreparationChannel(t, starter.entered)
	aborted := make(chan error, 1)
	go func() { _, e := m.AbortStartPreparation(context.Background(), id); aborted <- e }()
	waitPreparationChannel(t, owned.Done())
	next := id
	next.StartID = "later"
	next.JobGeneration = 8
	if _, _, e := m.BeginStartPreparation(t.Context(), next); !errors.Is(e, ErrAlreadyRunning) {
		t.Fatal("slot reused while process start pending", e)
	}
	close(starter.release)
	if e := <-prepared; e == nil {
		t.Fatal("canceled prepare succeeded")
	}
	if e := <-aborted; e != nil {
		t.Fatal(e)
	}
	if starter.delegate.processes[0].waits.Load() != 1 || starter.delegate.processes[0].kills.Load() != 1 {
		t.Fatal("in-flight child was not killed/reaped once")
	}
}
func TestStartPreparationSlowCleanupHoldsSlotAndDoesNotRepeatKill(t *testing.T) {
	m, starter, job, id := preparationManager(t, &preparationWitness{})
	p, owned, e := m.BeginStartPreparation(t.Context(), id)
	if e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	if e = m.SetPreparationCleanup(p, func() { close(entered); <-release }); e != nil {
		t.Fatal(e)
	}
	if _, e = m.PrepareStart(owned, p, job); e != nil {
		t.Fatal(e)
	}
	m.FinishStartPreparation(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan error, 1)
	go func() { _, e := m.AbortStartPreparation(ctx, id); ended <- e }()
	waitPreparationChannel(t, entered)
	cancel()
	if e := <-ended; !errors.Is(e, context.Canceled) {
		t.Fatal("cleanup outlived request budget", e)
	}
	if _, e := m.AbortStartPreparation(t.Context(), id); !errors.Is(e, ErrPreparationConflict) {
		t.Fatal("unfinished duplicate abort acknowledged", e)
	}
	next := id
	next.StartID = "next-cleanup"
	next.JobGeneration = 8
	if _, _, e := m.BeginStartPreparation(t.Context(), next); !errors.Is(e, ErrAlreadyRunning) {
		t.Fatal("cleanup slot released early", e)
	}
	close(release)
	waitPreparationChannel(t, p.cleanupDone)
	if starter.processes[0].kills.Load() != 1 {
		t.Fatal("blind repeated kill")
	}
}

func TestStartPreparationShutdownDrainsPreparedOwner(t *testing.T) {
	m, starter, job, id := preparationManager(t, &preparationWitness{})
	p := prepareFixture(t, m, job, id)
	if errs := m.StopAllAndDrain(t.Context()); len(errs) != 0 {
		t.Fatal(errs)
	}
	waitPreparationChannel(t, p.cleanupDone)
	if starter.processes[0].waits.Load() != 1 || !m.isDrained() {
		t.Fatal("shutdown left staged child")
	}
}
