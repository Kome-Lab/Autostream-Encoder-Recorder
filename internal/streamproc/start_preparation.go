package streamproc

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

var ErrPreparationConflict = errors.New("start preparation conflict")
var ErrPreparationUnknown = errors.New("start preparation unknown")

// Start ID and this captured pointer jointly own the slot and its resources.
type StartPreparationIdentity struct {
	StreamID         string `json:"stream_id"`
	StartID          string `json:"start_id"`
	EncoderServiceID string `json:"encoder_service_id"`
	JobGeneration    uint64 `json:"job_generation"`
	ArchiveRunID     string `json:"archive_run_id"`
}
type StartPreparationState struct {
	SchemaVersion int                      `json:"schema_version"`
	Identity      StartPreparationIdentity `json:"identity"`
	Phase         string                   `json:"phase"`
	ExpiresAt     *time.Time               `json:"expires_at,omitempty"`
	Process       *Snapshot                `json:"process,omitempty"`
	CoverState    *videocover.RuntimeState `json:"cover_state,omitempty"`
	Code          string                   `json:"code,omitempty"`
}
type StartPreparation struct {
	identity      StartPreparationIdentity
	tracked       *trackedProcess
	phase         string
	code          string
	expiresAt     time.Time
	frame         []byte
	cancel        context.CancelFunc
	cleanup       func()
	identityCheck func() bool
	abortStarted  bool
	cleanupOnce   sync.Once
	cleanupDone   chan struct{}
	exited        chan struct{}
	prepareDone   chan struct{}
}

func processOwnsSlot(p *trackedProcess) bool {
	if p.cleaning {
		return true
	}
	switch p.snapshot.Status {
	case "starting", "running", "stopping", "packaging":
		return true
	}
	return false
}
func preparationDiagnostic(id StartPreparationIdentity, event, phase string) {
	log.Printf("encoder start: event=%s stream_id=%s start_id=%s job_generation=%d phase=%s", event, id.StreamID, id.StartID, id.JobGeneration, phase)
}
func (m *Manager) BeginStartPreparation(ctx context.Context, id StartPreparationIdentity) (*StartPreparation, context.Context, error) {
	if id.StreamID == "" || id.StartID == "" || id.EncoderServiceID == "" || id.JobGeneration == 0 || id.ArchiveRunID == "" {
		return nil, nil, ErrPreparationConflict
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.preparations == nil {
		m.preparations = map[string]*StartPreparation{}
	}
	if m.preparations[id.StartID] != nil {
		return nil, nil, ErrPreparationConflict
	}
	for _, p := range m.processes {
		if processOwnsSlot(p) {
			return nil, nil, ErrAlreadyRunning
		}
	}
	previous, err := m.readCoverGeneration(id.StreamID)
	if err != nil {
		return nil, nil, err
	}
	// A lost receipt/restart cannot permit reuse of an already issued visual epoch.
	if previous.JobGeneration >= id.JobGeneration {
		return nil, nil, ErrPreparationConflict
	}
	child, cancel := context.WithCancel(ctx)
	p := &StartPreparation{identity: id, phase: "preparing", cancel: cancel, cleanupDone: make(chan struct{}), prepareDone: make(chan struct{})}
	p.tracked = &trackedProcess{snapshot: Snapshot{StreamID: id.StreamID, Status: "starting"}, preparation: p}
	if m.processes == nil {
		m.processes = map[string]*trackedProcess{}
	}
	m.processes[id.StreamID] = p.tracked
	m.preparations[id.StartID] = p
	preparationDiagnostic(id, "prepare_begin", "preparing")
	return p, child, nil
}

// FinishStartPreparation is called exactly once by the HTTP prepare owner.
func (m *Manager) FinishStartPreparation(p *StartPreparation) { close(p.prepareDone) }
func (m *Manager) SetPreparationCleanup(p *StartPreparation, cleanup func()) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.processes[p.identity.StreamID] != p.tracked || p.phase != "preparing" {
		return ErrPreparationConflict
	}
	p.cleanup = cleanup
	return nil
}
func (m *Manager) PrepareStart(ctx context.Context, p *StartPreparation, job lifecycle.StreamJob) (StartPreparationState, error) {
	if job.StreamID != p.identity.StreamID || job.ArchiveRunID != p.identity.ArchiveRunID || job.VideoCoverStart == nil || job.VideoCoverStart.JobGeneration != p.identity.JobGeneration {
		return StartPreparationState{}, ErrPreparationConflict
	}
	if _, err := m.startProcess(ctx, job, p); err != nil {
		return StartPreparationState{}, err
	}
	return m.StartPreparationStatus(p.identity.StreamID, p.identity.StartID)
}
func (m *Manager) findPreparationLocked(id StartPreparationIdentity) (*StartPreparation, error) {
	p := m.preparations[id.StartID]
	if p == nil {
		return nil, ErrPreparationUnknown
	}
	expected := p.identity
	if expected.StreamID != id.StreamID || expected.EncoderServiceID != id.EncoderServiceID || expected.JobGeneration != id.JobGeneration || (id.ArchiveRunID != "" && expected.ArchiveRunID != id.ArchiveRunID) {
		return nil, ErrPreparationConflict
	}
	return p, nil
}
func (m *Manager) preparationStateLocked(p *StartPreparation) StartPreparationState {
	s := StartPreparationState{SchemaVersion: 2, Identity: p.identity, Phase: p.phase, Code: p.code}
	if p.phase == "prepared" || p.phase == "committing" {
		v := p.expiresAt
		s.ExpiresAt = &v
	}
	if p.phase == "running" {
		snapshot := p.tracked.snapshot
		s.Process = &snapshot
		p.tracked.coverMu.Lock()
		cover := p.tracked.coverStateSnapshot()
		p.tracked.coverMu.Unlock()
		s.CoverState = &cover
	}
	return s
}
func (m *Manager) StartPreparationStatus(streamID, startID string) (StartPreparationState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.preparations[startID]
	if p == nil {
		return StartPreparationState{}, ErrPreparationUnknown
	}
	if p.identity.StreamID != streamID || p.phase == "preparing" {
		return StartPreparationState{}, ErrPreparationConflict
	}
	return m.preparationStateLocked(p), nil
}
func (m *Manager) rememberPreparationTerminalLocked(p *StartPreparation) {
	for _, id := range m.preparationOrder {
		if id == p.identity.StartID {
			return
		}
	}
	m.preparationOrder = append(m.preparationOrder, p.identity.StartID)
	for len(m.preparationOrder) > 64 {
		id := m.preparationOrder[0]
		m.preparationOrder = m.preparationOrder[1:]
		delete(m.preparations, id)
	}
}
func (m *Manager) expirePreparation(p *StartPreparation) {
	m.mu.Lock()
	if m.preparations[p.identity.StartID] != p || m.processes[p.identity.StreamID] != p.tracked || p.phase != "prepared" || time.Now().Before(p.expiresAt) {
		m.mu.Unlock()
		return
	}
	// Claim expiry under the same lock as commit before performing any IO.
	p.phase = "expired"
	p.tracked.cleaning = true
	p.cancel()
	m.mu.Unlock()
	_ = m.abortPreparation(context.Background(), p, "expired")
}

func (m *Manager) SetPreparationIdentityCheck(p *StartPreparation, check func() bool) {
	m.mu.Lock()
	p.identityCheck = check
	m.mu.Unlock()
}
func (m *Manager) FailStartPreparation(p *StartPreparation) error {
	return m.abortPreparation(context.Background(), p, "failed")
}

type preparationWitnessContextKey struct{}
