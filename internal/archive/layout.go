package archive

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
)

type Layout struct {
	RootDir  string
	StreamID string
}

func NewLayout(rootDir, streamID string) (Layout, error) {
	if strings.TrimSpace(rootDir) == "" {
		return Layout{}, errors.New("archive root dir is required")
	}
	if !isSafeID(streamID) {
		return Layout{}, errors.New("unsafe stream id")
	}
	return Layout{RootDir: filepath.Clean(rootDir), StreamID: streamID}, nil
}

func (l Layout) TmpDir() string {
	return filepath.Join(l.RootDir, "tmp", l.StreamID)
}

func (l Layout) FinalDir() string {
	return filepath.Join(l.RootDir, "final", l.StreamID)
}

func (l Layout) FinalMKV() string {
	return filepath.Join(l.TmpDir(), "final.mkv")
}

func (l Layout) FinalMP4() string {
	return filepath.Join(l.FinalDir(), "final.mp4")
}

func (l Layout) TmpMetadata() string {
	return filepath.Join(l.TmpDir(), "metadata.json")
}

func (l Layout) FinalMetadata() string {
	return filepath.Join(l.FinalDir(), "metadata.json")
}

func (l Layout) TmpLogs() string {
	return filepath.Join(l.TmpDir(), "logs.jsonl")
}

func (l Layout) TmpFFmpegProgress() string {
	return filepath.Join(l.TmpDir(), "ffmpeg-progress.txt")
}

func (l Layout) TmpFFmpegAudioStats() string {
	return filepath.Join(l.TmpDir(), "ffmpeg-audio-stats.txt")
}

func (l Layout) TmpDiscordOpus() string {
	return filepath.Join(l.TmpDir(), "discord-opus.jsonl")
}

func (l Layout) TmpDiscordOpusSDP() string {
	return filepath.Join(l.TmpDir(), "discord-opus.sdp")
}

func (l Layout) FinalLogs() string {
	return filepath.Join(l.FinalDir(), "logs.jsonl")
}

func (l Layout) TmpCaptions() string {
	return filepath.Join(l.TmpDir(), "captions.vtt")
}

func (l Layout) FinalCaptions() string {
	return filepath.Join(l.FinalDir(), "captions.vtt")
}

func (l Layout) TmpTranscript() string {
	return filepath.Join(l.TmpDir(), "transcript.json")
}

func (l Layout) FinalTranscript() string {
	return filepath.Join(l.FinalDir(), "transcript.json")
}

func isSafeID(id string) bool {
	if id == "" || strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}
