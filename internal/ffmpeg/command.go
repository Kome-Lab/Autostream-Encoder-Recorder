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

// Live encoding must keep wall-clock pace.  The previous veryfast preset was
// measured below realtime on the 1080p60 scene + watermark path, which made
// the provider's ingest buffer drain and caused viewer buffering warnings.
const liveVideoPreset = "superfast"

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
	return buildLiveArchiveArgsToOutputTargetWithRuntimeSettings(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath, 0, false, p)
}

func BuildLiveArchiveArgsToOutputTargetWithRuntimeSettings(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput string, audioGainDB float64, p EncoderProfile) []string {
	return buildLiveArchiveArgsToOutputTargetWithRuntimeSettings(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput, audioGainDB, true, p)
}

func buildLiveArchiveArgsToOutputTargetWithRuntimeSettings(inputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput string, audioGainDB float64, runtimeAudio bool, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	input := ResolveInputTarget(inputURL)
	args := []string{
		"-hide_banner", "-y", "-i", input,
	}
	if strings.TrimSpace(watermarkInput) != "" {
		args = append(args, watermarkInputArgs(watermarkInput)...)
		args = append(args,
			"-filter_complex", watermarkFilter(p),
			"-map", "[v]", "-map", "0:a:0?",
		)
	} else {
		args = append(args, "-map", "0:v:0?", "-map", "0:a:0?")
	}
	args = append(args, liveVideoCodecArgs(p)...)
	args = append(args, "-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate))
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	if runtimeAudio {
		args = append(args, "-filter:a", runtimeAudioFilter(audioGainDB, audioStatsPath))
	} else if audioStatsPath != "" {
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
	return buildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath, 0, false, p)
}

func BuildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput string, audioGainDB float64, p EncoderProfile) []string {
	return buildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput, audioGainDB, true, p)
}

func buildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput string, audioGainDB float64, runtimeAudio bool, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	input := ResolveInputTarget(audioSDPPath)
	// Discord-only jobs have no camera/video track. Render an audio-reactive
	// waveform over a dark slate background so the preview is visibly alive
	// instead of presenting a misleading black frame.
	filter := discordAudioFilterWithSilence(p)
	if runtimeAudio {
		filter = discordAudioRuntimeFilterWithSilence(p, audioGainDB)
	}
	args := []string{
		"-hide_banner", "-y",
		"-f", "lavfi", "-re", "-i", "color=c=0x0b1020:s=" + itoa(p.Width) + "x" + itoa(p.Height) + ":r=" + itoa(p.FPS),
		"-protocol_whitelist", "file,udp,rtp",
		"-i", filepath.Clean(input),
		// Discord/DAVE omits encrypted silence packets. Keep a continuous
		// audio clock so a mute never turns the live output into an ended
		// audio stream; real Opus is mixed over this silence when present.
		"-re", "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=" + itoa(p.SampleRate),
	}
	if strings.TrimSpace(watermarkInput) != "" {
		args = append(args, watermarkInputArgs(watermarkInput)...)
		if runtimeAudio {
			filter = discordAudioRuntimeWatermarkFilterWithSilence(p, audioGainDB)
		} else {
			filter = discordAudioWatermarkFilterWithSilence(p)
		}
	}
	audioMap := "[aout]"
	if strings.TrimSpace(audioStatsPath) != "" {
		filter, audioMap = appendComplexAudioStats(filter, audioStatsPath)
	}
	args = append(args, "-filter_complex", filter, "-map", "[v]", "-map", audioMap)
	args = append(args, liveVideoCodecArgs(p)...)
	args = append(args, "-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate))
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	return append(args, "-f", "tee", buildLiveTeeOutput(outputTarget, output, previewPlaylistPath))
}

// BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark
// combines Worker-rendered JPEG scene frames with the existing Bot Opus/RTP
// audio bridge. Worker does not encode video: SRT is terminated by the Go
// ingest layer and this final Encoder expands the image stream to the selected
// output FPS, applies the watermark, encodes once, and feeds the shared tee.
func BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(videoInputURL, audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath string, p EncoderProfile) []string {
	return buildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(videoInputURL, audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath, 0, false, p)
}

func BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(videoInputURL, audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput string, audioGainDB float64, p EncoderProfile) []string {
	return buildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(videoInputURL, audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput, audioGainDB, true, p)
}

func buildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(videoInputURL, audioSDPPath, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput string, audioGainDB float64, runtimeAudio bool, p EncoderProfile) []string {
	output := filepath.Clean(archivePath)
	videoInput := ResolveInputTarget(videoInputURL)
	audioInput := ResolveInputTarget(audioSDPPath)
	filter := workerSceneFilterWithSilence(workerSceneFilter(p))
	if runtimeAudio {
		filter = workerSceneRuntimeFilterWithSilence(workerSceneFilter(p), audioGainDB)
	}
	args := []string{
		"-hide_banner", "-y",
		// The Go ingest bridge writes a continuous concatenated JPEG stream. Use
		// FFmpeg's raw MJPEG demuxer rather than image2pipe's generic image
		// sequence probe so a live TCP stream remains consumable after frame one.
		"-thread_queue_size", "512", "-f", "mjpeg", "-framerate", "60", "-i", videoInput,
		"-thread_queue_size", "512", "-protocol_whitelist", "file,udp,rtp", "-i", filepath.Clean(audioInput),
		"-re", "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=" + itoa(p.SampleRate),
	}
	if strings.TrimSpace(watermarkInput) != "" {
		args = append(args, watermarkInputArgs(watermarkInput)...)
		if runtimeAudio {
			filter = workerSceneRuntimeFilterWithSilenceAndWatermark(p, audioGainDB)
		} else {
			filter = workerSceneFilterWithSilenceAndWatermark(p)
		}
	}
	audioMap := "[aout]"
	if strings.TrimSpace(audioStatsPath) != "" {
		filter, audioMap = appendComplexAudioStats(filter, audioStatsPath)
	}
	args = append(args, "-filter_complex", filter, "-map", "[v]", "-map", audioMap)
	args = append(args, liveVideoCodecArgs(p)...)
	args = append(args, "-c:a", "aac", "-b:a", p.AudioBitrate, "-ar", itoa(p.SampleRate))
	if progressPath != "" {
		args = append(args, "-nostats", "-progress", filepath.Clean(progressPath))
	}
	return append(args, "-f", "tee", buildLiveTeeOutput(outputTarget, output, previewPlaylistPath))
}

func watermarkFilter(p EncoderProfile) string {
	return "[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=rgba[base];[1:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func discordAudioFilter(p EncoderProfile) string {
	return discordAudioFilterWithSilence(p)
}

func discordAudioWatermarkFilter(p EncoderProfile) string {
	return discordAudioWatermarkFilterWithSilence(p)
}

func discordAudioFilterWithSilence(p EncoderProfile) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0,asplit=2[aout][wavein];[0:v]format=rgba[bg];[wavein]showwaves=s=" + itoa(p.Width) + "x" + itoa(p.Height/2) + ":mode=line:rate=30:colors=0x38bdf8,format=rgba[wave];[bg][wave]overlay=0:" + itoa(p.Height/4) + ":eof_action=repeat:repeatlast=1:shortest=0,format=yuv420p[v]"
}

func discordAudioWatermarkFilterWithSilence(p EncoderProfile) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0,asplit=2[aout][wavein];[0:v]format=rgba[bg];[wavein]showwaves=s=" + itoa(p.Width) + "x" + itoa(p.Height/2) + ":mode=line:rate=30:colors=0x38bdf8,format=rgba[wave];[bg][wave]overlay=0:" + itoa(p.Height/4) + ":eof_action=repeat:repeatlast=1:shortest=0,format=rgba[base];[3:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func discordAudioRuntimeFilterWithSilence(p EncoderProfile, audioGainDB float64) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0,volume@gain=" + formatGainDB(audioGainDB) + "dB,asplit=2[aout][wavein];[0:v]format=rgba[bg];[wavein]showwaves=s=" + itoa(p.Width) + "x" + itoa(p.Height/2) + ":mode=line:rate=30:colors=0x38bdf8,format=rgba[wave];[bg][wave]overlay=0:" + itoa(p.Height/4) + ":eof_action=repeat:repeatlast=1:shortest=0,format=yuv420p[v]"
}

func discordAudioRuntimeWatermarkFilterWithSilence(p EncoderProfile, audioGainDB float64) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0,volume@gain=" + formatGainDB(audioGainDB) + "dB,asplit=2[aout][wavein];[0:v]format=rgba[bg];[wavein]showwaves=s=" + itoa(p.Width) + "x" + itoa(p.Height/2) + ":mode=line:rate=30:colors=0x38bdf8,format=rgba[wave];[bg][wave]overlay=0:" + itoa(p.Height/4) + ":eof_action=repeat:repeatlast=1:shortest=0,format=rgba[base];[3:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func workerSceneFilter(p EncoderProfile) string {
	return "[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=yuv420p[v]"
}

func workerSceneWatermarkFilter(p EncoderProfile) string {
	return workerSceneFilterWithSilenceAndWatermark(p)
}

func workerSceneFilterWithSilence(videoFilter string) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0[aout];" + videoFilter
}

func workerSceneFilterWithSilenceAndWatermark(p EncoderProfile) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0[aout];[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=rgba[base];[3:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func workerSceneRuntimeFilterWithSilence(videoFilter string, audioGainDB float64) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0,volume@gain=" + formatGainDB(audioGainDB) + "dB[aout];" + videoFilter
}

func workerSceneRuntimeFilterWithSilenceAndWatermark(p EncoderProfile, audioGainDB float64) string {
	return "[1:a:0][2:a:0]amix=inputs=2:duration=longest:dropout_transition=0,volume@gain=" + formatGainDB(audioGainDB) + "dB[aout];[0:v]scale=" + itoa(p.Width) + ":" + itoa(p.Height) + ":force_original_aspect_ratio=decrease,pad=" + itoa(p.Width) + ":" + itoa(p.Height) + ":(ow-iw)/2:(oh-ih)/2:color=0x0b1020,setsar=1,format=rgba[base];[3:v]format=rgba,scale=" + itoa(p.Width) + ":" + itoa(p.Height) + "[wm];[base][wm]overlay=0:0:format=auto,format=yuv420p[v]"
}

func watermarkInputArgs(input string) []string {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(strings.ToLower(trimmed), "tcp://") {
		return []string{"-thread_queue_size", "8", "-f", "png_pipe", "-framerate", "2", "-i", trimmed}
	}
	return []string{"-loop", "1", "-i", filepath.Clean(trimmed)}
}

func runtimeAudioFilter(audioGainDB float64, audioStatsPath string) string {
	filter := "volume@gain=" + formatGainDB(audioGainDB) + "dB"
	if strings.TrimSpace(audioStatsPath) != "" {
		filter += "," + audioStatsFilter(audioStatsPath)
	}
	return filter
}

func formatGainDB(value float64) string {
	return strconv.FormatFloat(value, 'f', 1, 64)
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
	return "astats=metadata=1:reset=1:measure_perchannel=none:measure_overall=RMS_level+Peak_level,ametadata=print:file=" + filepath.ToSlash(filepath.Clean(path)) + ":direct=1:enable='not(mod(n,50))'"
}

func liveVideoCodecArgs(p EncoderProfile) []string {
	return []string{
		// Keep the coded geometry explicit at the output boundary. The complex
		// filter graph already normalizes frames, but tee/FLV consumers must not
		// be allowed to infer a square coded size from an upstream stream.
		"-s:v", itoa(p.Width) + "x" + itoa(p.Height), "-aspect", displayAspect(p.Width, p.Height),
		"-c:v", "libx264", "-preset", liveVideoPreset, "-tune", "zerolatency", "-pix_fmt", "yuv420p", "-b:v", p.VideoBitrate,
		"-minrate:v", p.VideoBitrate, "-maxrate:v", p.VideoBitrate, "-bufsize:v", cbrBufferSize(p.VideoBitrate),
		"-r", itoa(p.FPS), "-g", itoa(p.FPS * p.KeyframeSec), "-keyint_min", itoa(p.FPS * p.KeyframeSec), "-sc_threshold", "0",
		"-x264-params", "repeat-headers=1:open-gop=0:nal-hrd=cbr",
	}
}

func displayAspect(width, height int) string {
	divisor := greatestCommonDivisor(width, height)
	if divisor <= 0 {
		return itoa(width) + ":" + itoa(height)
	}
	return itoa(width/divisor) + ":" + itoa(height/divisor)
}

func greatestCommonDivisor(left, right int) int {
	if left < 0 {
		left = -left
	}
	if right < 0 {
		right = -right
	}
	for right != 0 {
		left, right = right, left%right
	}
	return left
}

func appendComplexAudioStats(filter, path string) (string, string) {
	return filter + ";[aout]" + audioStatsFilter(path) + "[aout_stats]", "[aout_stats]"
}
