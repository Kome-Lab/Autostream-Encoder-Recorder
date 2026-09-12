package streamproc

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

// OutputRelayPolicy returns the explicit non-secret v2 routing configuration.
func (m *Manager) OutputRelayPolicy() outputrelay.Policy {
	return outputrelay.NewWithRequireRelay(m.OutputRelayURL, m.OutputRelayMode, m.OutputRelayBindingID, m.RequireOutputRelay)
}

// AuthorizeOutputRelay must be called before resolving a YouTube key, an input
// target, or starting FFmpeg.  It is shared with the HTTP layer to keep the
// direct and managed-Live-API acceptance rules identical.
func (m *Manager) AuthorizeOutputRelay(job lifecycle.StreamJob) (bool, error) {
	return m.OutputRelayPolicy().AuthorizeYouTubeOutput(job.YouTubeOutputMode, job.YouTubeOutputReady, job.OutputRelayBindingID)
}

func clearUnusedYouTubeOutputTarget(job *lifecycle.StreamJob) {
	if job == nil {
		return
	}
	job.RTMPURL = ""
	job.StreamKey = ""
	job.StreamKeySecretName = ""
}

func (m *Manager) validateInputForLayout(job lifecycle.StreamJob, layout archive.Layout) error {
	if job.InputMode == "worker_scene_frames_srt" {
		if !strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_worker_video:") {
			return ffmpeg.ErrUnsafeInputTarget
		}
		if !strings.HasPrefix(strings.TrimSpace(job.AudioInputURL), "internal_discord_audio:") {
			return ffmpeg.ErrUnsafeInputTarget
		}
		got := filepath.Clean(ffmpeg.ResolveInputTarget(job.AudioInputURL))
		want := filepath.Clean(layout.TmpDiscordOpusSDP())
		if got != want {
			return ffmpeg.ErrUnsafeInputTarget
		}
		return nil
	}
	if job.InputMode == "discord_opus_rtp" {
		if !strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_discord_audio:") {
			return ffmpeg.ErrUnsafeInputTarget
		}
		got := filepath.Clean(ffmpeg.ResolveInputTarget(job.InputURL))
		want := filepath.Clean(layout.TmpDiscordOpusSDP())
		if got != want {
			return ffmpeg.ErrUnsafeInputTarget
		}
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_discord_audio:") {
		return ffmpeg.ErrUnsafeInputTarget
	}
	if strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_worker_video:") || strings.TrimSpace(job.AudioInputURL) != "" {
		return ffmpeg.ErrUnsafeInputTarget
	}
	return nil
}

func (m *Manager) liveOutputTarget(job lifecycle.StreamJob) (string, error) {
	policy := m.OutputRelayPolicy()
	if err := policy.ValidateConfiguration(); err != nil {
		return "", err
	}
	if !policy.UsesLocalRelay() {
		if m.RequireOutputRelay {
			return "", errors.New("output relay URL is required")
		}
		return job.RTMPURL + "/" + job.StreamKey, nil
	}
	target := relayOutputTarget(policy.URL, job.StreamID)
	if err := ffmpeg.ValidateRelayOutputTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

func relayOutputTarget(template, streamID string) string {
	template = strings.TrimSpace(template)
	escapedStreamID := url.PathEscape(strings.TrimSpace(streamID))
	if strings.Contains(template, "{stream_id}") {
		return strings.ReplaceAll(template, "{stream_id}", escapedStreamID)
	}
	return strings.TrimRight(template, "/") + "/" + escapedStreamID
}
