package streamproc

import (
	"strings"
	"testing"
)

func TestBoundedStderrKeepsOnlyDiagnosticTail(t *testing.T) {
	capture := &boundedStderr{}
	input := strings.Repeat("x", maxFFmpegStderrBytes) + "Connection refused"
	if written, err := capture.Write([]byte(input)); err != nil || written != len(input) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", written, err, len(input))
	}
	got := capture.String()
	if !strings.HasPrefix(got, "[truncated] ") {
		t.Fatalf("diagnostic was not marked truncated: %q", got)
	}
	if !strings.Contains(got, "Connection refused") {
		t.Fatalf("diagnostic tail lost the useful failure: %q", got)
	}
	if len(got) > maxFFmpegStderrBytes+len("[truncated] ") {
		t.Fatalf("diagnostic exceeded bounded size: %d", len(got))
	}
}

func TestExecProcessCommandUsesImmediateFFmpegFilterCommandSyntax(t *testing.T) {
	stdin := &recordingWriteCloser{}
	process := &execProcess{stdin: stdin}
	if err := process.Command("volume@gain", "volume", "6.5dB"); err != nil {
		t.Fatal(err)
	}
	if got, want := stdin.String(), "cvolume@gain -1 volume 6.5dB\n"; got != want {
		t.Fatalf("ffmpeg command input=%q, want %q", got, want)
	}
}
