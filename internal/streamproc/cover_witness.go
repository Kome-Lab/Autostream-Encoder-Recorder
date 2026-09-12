package streamproc

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/watermarkfeed"
)

// CoverGraphWitness is the narrow graph-observation seam. Production waits
// for the exact feed version to be consumed and then for output progress to
// advance. Tests can provide a deterministic witness without launching
// FFmpeg.
type CoverGraphWitness interface {
	Apply(ctx context.Context, source *imagefeed.Source, frame []byte, initial bool, progressPath string) error
}

// WatermarkGraphWitness mirrors the Cover graph witness for the independently
// controlled Watermark feed. A successful source update alone is not enough:
// production also waits for downstream output progress before the new layer
// revision can appear in applied_witness.watermark.
type WatermarkGraphWitness interface {
	Apply(ctx context.Context, source *watermarkfeed.Source, frame []byte, progressPath string) error
}

func (m *Manager) coverApplyTimeout() time.Duration {
	if m.CoverApplyTimeout > 0 {
		return m.CoverApplyTimeout
	}
	return 6 * time.Second
}

func (m *Manager) coverFetchTimeout() time.Duration {
	if m.CoverFetchTimeout > 0 {
		return m.CoverFetchTimeout
	}
	return 10 * time.Second
}

func (m *Manager) coverGraphWitness() CoverGraphWitness {
	if m.CoverWitness != nil {
		return m.CoverWitness
	}
	return progressCoverWitness{}
}

func (m *Manager) watermarkGraphWitness() WatermarkGraphWitness {
	if m.WatermarkWitness != nil {
		return m.WatermarkWitness
	}
	return progressWatermarkWitness{}
}

type progressCoverWitness struct{}

func (progressCoverWitness) Apply(ctx context.Context, source *imagefeed.Source, frame []byte, initial bool, progressPath string) error {
	var err error
	if initial {
		err = source.WaitInitialDelivery(ctx)
	} else {
		err = source.UpdateAndWait(ctx, frame)
	}
	if err != nil {
		return err
	}
	return waitForVisualOutputAdvance(ctx, progressPath)
}

type progressWatermarkWitness struct{}

func (progressWatermarkWitness) Apply(ctx context.Context, source *watermarkfeed.Source, frame []byte, progressPath string) error {
	if err := source.UpdateAndWait(ctx, frame); err != nil {
		return err
	}
	return waitForVisualOutputAdvance(ctx, progressPath)
}

func waitForVisualOutputAdvance(ctx context.Context, progressPath string) error {
	// Establish the output baseline only after the exact feed version was
	// delivered. Progress that happened while the socket write was pending is
	// not evidence that this visual-layer revision crossed the graph.
	before := readCoverProgress(progressPath)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := readCoverProgress(progressPath)
		if current.Frame > before.Frame && current.OutTimeUS > before.OutTimeUS {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func readCoverProgress(path string) ffmpeg.Progress {
	body, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ffmpeg.Progress{}
	}
	return ffmpeg.ParseProgress(string(body))
}
