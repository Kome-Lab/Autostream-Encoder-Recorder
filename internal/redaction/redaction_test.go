package redaction

import (
	"strings"
	"testing"
)

func TestArgsRedactsStreamKeyAndMasksURLCredentials(t *testing.T) {
	args := []string{
		"-i",
		"rtsp://user:camera-password@camera.example.com/live",
		"-f",
		"tee",
		"[f=flv]rtmps://youtube.example.com/live2/youtube-secret-key|[f=matroska]/archives/final.mkv",
	}
	redacted := Args(args, "youtube-secret-key", "rtsp://user:camera-password@camera.example.com/live")
	joined := strings.Join(redacted, " ")
	if strings.Contains(joined, "camera-password") || strings.Contains(joined, "youtube-secret-key") {
		t.Fatalf("secret leaked in args: %#v", redacted)
	}
	if !strings.Contains(joined, "rtsp://camera.example.com/<REDACTED>") || !strings.Contains(joined, "<REDACTED>") {
		t.Fatalf("expected masked URL and redacted stream key: %#v", redacted)
	}
}

func TestMessageRedactsSensitiveValuesAndMasksURLs(t *testing.T) {
	message := "ffmpeg failed for rtsp://user:camera-password@camera.example.com/live and rtmps://youtube.example.com/live2/hidden-stream-key"
	redacted := Message(message, "hidden-stream-key", "rtsp://user:camera-password@camera.example.com/live", "rtmps://youtube.example.com/live2/hidden-stream-key")
	if strings.Contains(redacted, "camera-password") || strings.Contains(redacted, "hidden-stream-key") {
		t.Fatalf("secret leaked in message: %s", redacted)
	}
	if !strings.Contains(redacted, "rtsp://camera.example.com/<REDACTED>") || !strings.Contains(redacted, "rtmps://youtube.example.com/<REDACTED>") {
		t.Fatalf("expected masked URLs in message: %s", redacted)
	}
}

func TestDiagnosticMasksUnknownURLsAndCredentialShapedValues(t *testing.T) {
	message := "Authorization: Bearer bearer-secret rtmps://youtube.example.com/live2/stream-key ast_ingest_v1.payload.signature"
	redacted := Diagnostic(message)
	for _, secret := range []string{"bearer-secret", "stream-key", "ast_ingest_v1.payload.signature"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "rtmps://youtube.example.com/<REDACTED>") {
		t.Fatalf("expected masked URL in diagnostic: %s", redacted)
	}
}

func TestMaskSensitiveURLKeepsOnlySchemeAndHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "RTSP path token and userinfo", raw: "rtsp://camera:camera-password@camera.example.com/live/path-token", want: "rtsp://camera.example.com/<REDACTED>"},
		{name: "SRT path token and query", raw: "srt://source.example.com:9000/path-token?mode=caller&passphrase=query-token", want: "srt://source.example.com:9000/<REDACTED>"},
		{name: "HLS path token and fragment", raw: "https://cdn.example.com/live/path-token/index.m3u8#fragment-token", want: "https://cdn.example.com/<REDACTED>"},
		{name: "percent encoded path token", raw: "rtsp://camera.example.com/live/%73%65%63%72%65%74-token", want: "rtsp://camera.example.com/<REDACTED>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MaskSensitiveURL(tc.raw)
			if !ok {
				t.Fatal("expected URL to be masked")
			}
			if got != tc.want {
				t.Fatalf("masked URL = %q, want %q", got, tc.want)
			}
			for _, secret := range []string{"camera-password", "path-token", "query-token", "fragment-token", "%73%65%63%72%65%74-token", "secret-token"} {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q leaked in masked URL %q", secret, got)
				}
			}
		})
	}
}
