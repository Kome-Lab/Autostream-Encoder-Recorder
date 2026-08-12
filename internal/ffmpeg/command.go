package ffmpeg

import (
	"path/filepath"
	"strconv"
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
	return BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, "", p)
}

func BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath string, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	input := ResolveInputTarget(inputURL)
	args := []string{
		"-hide_banner", "-y", "-i", input,
	}
	if strings.TrimSpace(watermarkPath) != "" {
		args = append(args,
			"-loop", "1", "-i", filepath.Clean(watermarkPath),
			"-filter_complex", watermarkFilter(p),
			"-map", "[v]", "-map", "0:a:0?",
		)
	} else {
		args = append(args, "-map", "0:v:0?", "-map", "0:a:0?")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-b:v", p.VideoBitrate,
		"-minrate:v", p.VideoBitrate, "-maxrate:v", p.VideoBitrate, "-bufsize:v", cbrBufferSize(p.VideoBitrate),
		"-r", itoa(p.FPS), "-g", itoa(p.FPS*p.KeyframeSec), "-keyint_min", itoa(p.FPS*p.KeyframeSec), "-sc_threshold", "0",
		"-x264-params", "repeat-headers=1:open-gop=0:nal-hrd=cbr",
		"-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate),
	)
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
	return BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, "", p)
}

func BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath string, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	input := ResolveInputTarget(audioSDPPath)
	// Discord-only jobs have no camera/video track. Render an audio-reactive
	// waveform over a dark slate background so the preview is visibly alive
	// instead of presenting a misleading black frame.
	filter := discordAudioFilter(p)
	args := []string{
		"-hide_banner", "-y",
		"-f", "lavfi", "-re", "-i", "color=c=0x0b1020:s=" + itoa(p.Width) + "x" + itoa(p.Height) + ":r=" + itoa(p.FPS),
		"-protocol_whitelist", "file,udp,rtp",
		"-i", filepath.Clean(input),
	}
	if strings.TrimSpace(watermarkPath) != "" {
		args = append(args, "-loop", "1", "-i", filepath.Clean(watermarkPath))
		filter = discordAudioWatermarkFilter(p)
	}
	args = append(args,
		"-filter_complex", filter,
		"-map", "[v]", "-map", "1:a:0",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-b:v", p.VideoBitrate,
		"-minrate:v", p.VideoBitrate, "-maxrate:v", p.VideoBitrate, "-bufsize:v", cbrBufferSize(p.VideoBitrate),
		"-r", itoa(p.FPS), "-g", itoa(p.FPS*p.KeyframeSec), "-keyint_min", itoa(p.FPS*p.KeyframeSec), "-sc_threshold", "0",
		"-x264-params", "repeat-headers=1:open-gop=0:nal-hrd=cbr",
		"-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate),
	)
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	if audioStatsPath != "" {
		args = append(args, "-filter:a", audioStatsFilter(audioStatsPath))
	}
	return append(args, "-f", "tee", buildLiveTeeOutput(outputTarget, output, previewPlaylistPath))
}

// BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark
// combines a Worker-rendered, video-only MPEG-TS scene with the existing Bot
// Opus/RTP audio bridge. SRT is terminated by the Go ingest layer, so FFmpeg
// only receives a credential-free loopback TCP endpoint.
func BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(videoInputURL, audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath string, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	videoInput := ResolveInputTarget(videoInputURL)
	audioInput := ResolveInputTarget(audioSDPPath)
	filter := workerSceneFilter(p)
	args := []string{
		"-hide_banner", "-y",
		"-thread_queue_size", "512", "-f", "mpegts", "-i", videoInput,
		"-thread_queue_size", "512", "-protocol_whitelist", "file,udp,rtp", "-i", filepath.Clean(audioInput),
	}
	if strings.TrimSpace(watermarkPath) != "" {
		args = append(args, "-loop", "1", "-i", filepath.Clean(watermarkPath))
		filter = workerSceneWatermarkFilter(p)
	}
	args = append(args,
		"-filter_complex", filter,
		"-map", "[v]", "-map", "1:a:0",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-b:v", p.VideoBitrate,
		"-minrate:v", p.VideoBitrate, "-maxrate:v", p.VideoBitrate, "-bufsize:v", cbrBufferSize(p.VideoBitrate),
		"-r", itoa(p.FPS), "-g", itoa(p.FPS*p.KeyframeSec), "-keyint_min", itoa(p.FPS*p.KeyframeSec), "-sc_threshold", "0",
		"-x264-params", "repeat-headers=1:open-gop=0:nal-hrd=cbr",
		"-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate),
	)
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	if audioStatsPath != "" {
		args = append(args, "-filter:a", audioStatsFilter(audioStatsPath))
	}
	return append(args, "-f", "tee", buildLiveTeeOutput(outputTarget, output, previewPlaylistPath))
}

func watermarkFilter(p EncoderProfile) string {
	return "[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=rgba[base];[1:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func discordAudioFilter(p EncoderProfile) string {
	return "[0:v]format=rgba[bg];[1:a]showwaves=s=" + itoa(p.Width) + "x" + itoa(p.Height/2) + ":mode=line:rate=30:colors=0x38bdf8,format=rgba[wave];[bg][wave]overlay=0:" + itoa(p.Height/4) + ":shortest=1,format=yuv420p[v]"
}

func discordAudioWatermarkFilter(p EncoderProfile) string {
	return "[0:v]format=rgba[bg];[1:a]showwaves=s=" + itoa(p.Width) + "x" + itoa(p.Height/2) + ":mode=line:rate=30:colors=0x38bdf8,format=rgba[wave];[bg][wave]overlay=0:" + itoa(p.Height/4) + ":shortest=1,format=rgba[base];[2:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func workerSceneFilter(p EncoderProfile) string {
	return "[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=yuv420p[v]"
}

func workerSceneWatermarkFilter(p EncoderProfile) string {
	return "[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=rgba[base];[2:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func buildLiveTeeOutput(outputTarget, archivePath, previewPlaylistPath string) string {
	slaves := []string{
		// A transient live-provider or relay failure must not tear down the
		// local recording and preview outputs.  The live output is optional;
		// the archive is the durable source used by the Control Panel.
		"[f=flv:onfail=ignore]" + escapeTeeSlaveURL(outputTarget),
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
		// Keep every segment for the lifetime of the active stream. The
		// archive tmp directory is removed by the existing stop cleanup, so
		// the preview can seek back to the beginning without a second muxer.
		"hls_list_size=0",
		"hls_flags=independent_segments+temp_file",
		"hls_segment_filename=" + escapeTeeOptionValue(segmentPattern),
	}
	slaves = append(slaves, "["+strings.Join(options, ":")+"]"+escapeTeeSlaveURL(playlist))
	return strings.Join(slaves, "|")
}

func cbrBufferSize(bitrate string) string {
	value := strings.TrimSpace(bitrate)
	if value == "" {
		return value
	}
	suffix := ""
	number := value
	if last := value[len(value)-1:]; strings.ContainsAny(last, "kKmMgG") {
		suffix = last
		number = value[:len(value)-1]
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil || parsed <= 0 {
		return value
	}
	doubled := parsed * 2
	if doubled == float64(int64(doubled)) {
		return strconv.FormatInt(int64(doubled), 10) + suffix
	}
	return strconv.FormatFloat(doubled, 'f', -1, 64) + suffix
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
