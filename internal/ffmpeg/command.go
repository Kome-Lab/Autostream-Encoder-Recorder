package ffmpeg

import (
	"path/filepath"
	"strings"
	"unicode"
)

type EncoderProfile struct {
	Width        int
	Height       int
	FPS          int
	VideoBitrate string
	AudioBitrate string
	SampleRate   int
	KeyframeSec  int
}

func DefaultProfile() EncoderProfile {
	return EncoderProfile{Width: 1920, Height: 1080, FPS: 60, VideoBitrate: "8000k", AudioBitrate: "160k", SampleRate: 48000, KeyframeSec: 2}
}

func BuildLiveArchiveArgs(inputURL, rtmpURL, streamKey, archivePath string, p EncoderProfile) []string {
	return BuildLiveArchiveArgsWithProgress(inputURL, rtmpURL, streamKey, archivePath, "", p)
}

func BuildLiveArchiveArgsWithProgress(inputURL, rtmpURL, streamKey, archivePath, progressPath string, p EncoderProfile) []string {
	return BuildLiveArchiveArgsWithTelemetry(inputURL, rtmpURL, streamKey, archivePath, progressPath, "", p)
}

func BuildLiveArchiveArgsWithTelemetry(inputURL, rtmpURL, streamKey, archivePath, progressPath, audioStatsPath string, p EncoderProfile) []string {
	return BuildLiveArchiveArgsToOutputTargetWithTelemetry(inputURL, rtmpURL+"/"+streamKey, archivePath, progressPath, audioStatsPath, p)
}

func BuildLiveArchiveArgsToOutputTargetWithTelemetry(inputURL, outputTarget, archivePath, progressPath, audioStatsPath string, p EncoderProfile) []string {
	return BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(inputURL, outputTarget, archivePath, "", progressPath, audioStatsPath, p)
}

func BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath string, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	input := ResolveInputTarget(inputURL)
	args := []string{
		"-hide_banner", "-y", "-i", input,
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "veryfast", "-b:v", p.VideoBitrate,
		"-r", itoa(p.FPS), "-g", itoa(p.FPS * p.KeyframeSec),
		"-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate),
	}
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	if audioStatsPath != "" {
		args = append(args, "-filter:a", audioStatsFilter(audioStatsPath))
	}
	return append(args, "-f", "tee", buildLiveTeeOutput(outputTarget, output, previewPlaylistPath))
}

func BuildDiscordAudioLiveArchiveArgsWithProgress(audioSDPPath, rtmpURL, streamKey, archivePath, progressPath string, p EncoderProfile) []string {
	return BuildDiscordAudioLiveArchiveArgsWithTelemetry(audioSDPPath, rtmpURL, streamKey, archivePath, progressPath, "", p)
}

func BuildDiscordAudioLiveArchiveArgsWithTelemetry(audioSDPPath, rtmpURL, streamKey, archivePath, progressPath, audioStatsPath string, p EncoderProfile) []string {
	return BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetry(audioSDPPath, rtmpURL+"/"+streamKey, archivePath, progressPath, audioStatsPath, p)
}

func BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetry(audioSDPPath, outputTarget, archivePath, progressPath, audioStatsPath string, p EncoderProfile) []string {
	return BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(audioSDPPath, outputTarget, archivePath, "", progressPath, audioStatsPath, p)
}

func BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath string, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	input := ResolveInputTarget(audioSDPPath)
	args := []string{
		"-hide_banner", "-y",
		"-f", "lavfi", "-re", "-i", "color=c=black:s=" + itoa(p.Width) + "x" + itoa(p.Height) + ":r=" + itoa(p.FPS),
		"-protocol_whitelist", "file,udp,rtp",
		"-i", filepath.Clean(input),
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-b:v", p.VideoBitrate,
		"-r", itoa(p.FPS), "-g", itoa(p.FPS * p.KeyframeSec),
		"-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate),
	}
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	if audioStatsPath != "" {
		args = append(args, "-filter:a", audioStatsFilter(audioStatsPath))
	}
	return append(args, "-f", "tee", buildLiveTeeOutput(outputTarget, output, previewPlaylistPath))
}

func buildLiveTeeOutput(outputTarget, archivePath, previewPlaylistPath string) string {
	slaves := []string{
		"[f=flv]" + escapeTeeSlaveURL(outputTarget),
		"[f=matroska]" + escapeTeeSlaveURL(filepath.ToSlash(filepath.Clean(archivePath))),
	}
	if strings.TrimSpace(previewPlaylistPath) == "" {
		return strings.Join(slaves, "|")
	}
	playlist := filepath.ToSlash(filepath.Clean(previewPlaylistPath))
	segmentPattern := filepath.ToSlash(filepath.Join(filepath.Dir(previewPlaylistPath), "segment-%06d.ts"))
	options := []string{
		"f=hls",
		"onfail=ignore",
		"use_fifo=1",
		"fifo_options=" + escapeTeeOptionValue("queue_size=1200:drop_pkts_on_overflow=1"),
		"hls_time=2",
		"hls_list_size=6",
		"hls_delete_threshold=1",
		"hls_flags=delete_segments+independent_segments+temp_file",
		"hls_segment_filename=" + escapeTeeOptionValue(segmentPattern),
	}
	slaves = append(slaves, "["+strings.Join(options, ":")+"]"+escapeTeeSlaveURL(playlist))
	return strings.Join(slaves, "|")
}

func escapeTeeOptionValue(value string) string {
	return escapeFFmpegToken(escapeFFmpegToken(value, ":]"), "|")
}

func escapeTeeSlaveURL(value string) string {
	return escapeFFmpegToken(value, "|")
}

func escapeFFmpegToken(value, terminators string) string {
	var escaped strings.Builder
	for _, char := range value {
		if char == '\\' || char == '\'' || strings.ContainsRune(terminators, char) || unicode.IsSpace(char) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}
	return escaped.String()
}

// RedactTeePath replaces path spellings used both as tee slave URLs and as
// second-level escaped tee option values.
func RedactTeePath(value, path, replacement string) string {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	value = RedactTeeValue(value, cleaned, replacement)
	return strings.ReplaceAll(value, path, replacement)
}

// RedactTeeValue replaces raw and tee-escaped representations of a value.
func RedactTeeValue(value, sensitive, replacement string) string {
	variants := []string{escapeTeeOptionValue(sensitive), escapeTeeSlaveURL(sensitive), sensitive}
	for _, variant := range variants {
		if variant != "" {
			value = strings.ReplaceAll(value, variant, replacement)
		}
	}
	return value
}

func BuildRemuxArgs(inputMKV, outputMP4 string) []string {
	return []string{"-hide_banner", "-y", "-i", filepath.Clean(inputMKV), "-c", "copy", "-movflags", "+faststart", filepath.Clean(outputMP4)}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	out := ""
	for v > 0 {
		out = string(rune('0'+v%10)) + out
		v /= 10
	}
	return out
}

func audioStatsFilter(path string) string {
	return "astats=metadata=1:reset=1,ametadata=print:file=" + filepath.ToSlash(filepath.Clean(path))
}
