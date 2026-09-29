package streamproc

import (
	"context"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

func (m *Manager) CommitStartPreparation(ctx context.Context, id StartPreparationIdentity) (StartPreparationState, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	m.mu.Lock()
	p, err := m.findPreparationLocked(id)
	if err != nil {
		m.mu.Unlock()
		return StartPreparationState{}, err
	}
	check := p.identityCheck
	m.mu.Unlock()
	if check != nil && !check() {
		return StartPreparationState{}, ErrPreparationConflict
	}
	m.mu.Lock()
	if p.phase == "running" && m.processes[id.StreamID] == p.tracked && p.tracked.snapshot.Status == "running" {
		s := m.preparationStateLocked(p)
		m.mu.Unlock()
		return s, nil
	}
	if p.phase != "prepared" || m.processes[id.StreamID] != p.tracked || !time.Now().Before(p.expiresAt) {
		m.mu.Unlock()
		return StartPreparationState{}, ErrPreparationConflict
	}
	p.phase = "committing"
	witnessCtx, witnessCancel := context.WithTimeout(ctx, m.coverApplyTimeout())
	p.cancel = witnessCancel
	tracked := p.tracked
	source := tracked.cover
	frame := p.frame
	progress := tracked.progressPath
	m.mu.Unlock()
	preparationDiagnostic(id, "commit_begin", "committing")
	witnessCtx = context.WithValue(witnessCtx, preparationWitnessContextKey{}, id)
	err = m.coverGraphWitness().Apply(witnessCtx, source, frame, true, progress)
	witnessCancel()
	if err == nil {
		err = ctx.Err()
	}
	m.mu.Lock()
	if err == nil && (m.processes[id.StreamID] != tracked || p.phase != "committing" || tracked.snapshot.Status != "starting") {
		err = ErrPreparationConflict
	}
	if err != nil {
		switch {
		case strings.HasPrefix(err.Error(), "initial_feed_delivery:"):
			p.code = "start_preparation_feed_witness_failed"
		case strings.HasPrefix(err.Error(), "output_advance:"):
			p.code = "start_preparation_output_witness_failed"
		default:
			p.code = "start_preparation_commit_failed"
		}
		diagnosticCode := p.code
		m.mu.Unlock()
		preparationDiagnostic(id, diagnosticCode, "committing")
		_ = m.abortPreparation(context.Background(), p, "failed")
		return StartPreparationState{}, err
	}
	tracked.coverMu.Lock()
	desired := tracked.job.VideoCoverStart
	markCoverApplied(&tracked.coverState, desired.Revision, desired.Active, desired.CoverAsset, tracked.watermarkState)
	tracked.coverMu.Unlock()
	tracked.snapshot.Status = "running"
	p.phase = "running"
	p.frame = nil
	p.cancel = func() {}
	result := m.preparationStateLocked(p)
	m.mu.Unlock()
	preparationDiagnostic(id, "commit_success", "running")
	layout, _ := archive.NewLayout(m.archiveRoot(), id.StreamID)
	go m.monitor(id.StreamID, layout.FinalMKV(), layout.TmpFFmpegProgress(), layout.TmpFFmpegAudioStats())
	return result, nil
}
func (m *Manager) AbortStartPreparation(ctx context.Context, id StartPreparationIdentity) (StartPreparationState, error) {
	m.mu.Lock()
	p, err := m.findPreparationLocked(id)
	if err != nil {
		m.mu.Unlock()
		return StartPreparationState{}, err
	}
	if p.phase == "aborted" && !p.tracked.cleaning {
		s := m.preparationStateLocked(p)
		m.mu.Unlock()
		return s, nil
	}
	if m.processes[id.StreamID] != p.tracked {
		m.mu.Unlock()
		return StartPreparationState{}, ErrPreparationConflict
	}
	m.mu.Unlock()
	if err := m.abortPreparation(ctx, p, "aborted"); err != nil {
		return StartPreparationState{}, err
	}
	return m.StartPreparationStatus(id.StreamID, id.StartID)
}
func (m *Manager) abortPreparation(ctx context.Context, p *StartPreparation, phase string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.mu.Lock()
	if m.processes[p.identity.StreamID] != p.tracked {
		m.mu.Unlock()
		return ErrPreparationConflict
	}
	select {
	case <-p.cleanupDone:
		select {
		case <-p.prepareDone:
			if !p.tracked.cleaning {
				p.phase = phase
				m.mu.Unlock()
				return nil
			}
		default:
		}
	default:
	}
	if p.abortStarted {
		m.mu.Unlock()
		return ErrPreparationConflict
	}
	p.abortStarted = true
	p.tracked.cleaning = true
	p.phase = phase
	p.cancel()
	m.mu.Unlock()
	select {
	case <-p.prepareDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	process := p.tracked.process
	exited := p.exited
	m.mu.Unlock()
	if process != nil {
		select {
		case <-exited:
		default:
			_ = process.Kill()
		}
		select {
		case <-exited:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.cleanupPreparationResources(p)
	select {
	case <-p.cleanupDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if m.processes[p.identity.StreamID] == p.tracked {
		scrubTrackedProcessJob(p.tracked)
		p.tracked.snapshot.Status = "failed"
		p.tracked.cleaning = false
		p.frame = nil
		p.phase = phase
		m.rememberPreparationTerminalLocked(p)
	}
	m.mu.Unlock()
	preparationDiagnostic(p.identity, "abort_end", phase)
	return nil
}

// Cleanup is started once and observed separately, so its IO cannot extend an
// abort request beyond the five-second budget. The slot stays owned until done.
func (m *Manager) cleanupPreparationResources(p *StartPreparation) {
	p.cleanupOnce.Do(func() {
		go func() {
			m.mu.Lock()
			tracked := p.tracked
			tracked.coverMu.Lock()
			cover := tracked.cover
			tracked.cover = nil
			tracked.coverMu.Unlock()
			watermark := tracked.watermark
			tracked.watermark = nil
			cleanup := p.cleanup
			m.mu.Unlock()
			if cover != nil {
				_ = cover.Close()
			}
			if watermark != nil {
				_ = watermark.Close()
			}
			if cleanup != nil {
				cleanup()
			}
			close(p.cleanupDone)
		}()
	})
}
