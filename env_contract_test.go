package encoderrecorder_test

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestEnvExampleUsesPanelManagedNodeCredentials(t *testing.T) {
	body, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	active := activeEnvKeys(string(body))
	allowed := map[string]bool{
		"AUTOSTREAM_NODE_CONFIG":      true,
		"AUTOSTREAM_ENV":              true,
		"AUTOSTREAM_BIND_ADDR":        true,
		"AUTOSTREAM_OUTPUT_RELAY_URL": true,
	}
	for key := range active {
		if !allowed[key] {
			t.Errorf("%s is not part of the minimal standard env contract", key)
		}
	}
	for key := range allowed {
		if !active[key] {
			t.Errorf("%s must remain an active host setting", key)
		}
	}
}

func TestBaseComposeOverridesHostOnlyBindAddress(t *testing.T) {
	body, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(body)
	for _, required := range []string{"AUTOSTREAM_BIND_ADDR: 0.0.0.0:8080", `- "8081:8080"`, "./config:/etc/autostream-encoder-recorder"} {
		if !strings.Contains(compose, required) {
			t.Errorf("base compose is missing %q", required)
		}
	}
}

func TestLocalComposeDoesNotRestoreLegacyCredentialInputs(t *testing.T) {
	body, err := os.ReadFile("docker-compose.local.yml")
	if err != nil {
		t.Fatal(err)
	}
	local := string(body)
	for _, required := range []string{"AUTOSTREAM_ENV: development", `AUTOSTREAM_OUTPUT_RELAY_URL: ""`} {
		if !strings.Contains(local, required) {
			t.Errorf("local compose is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"SERVICE_CONTROL_TOKEN",
		"ENCODER_WORKER_EVENTS_TOKEN",
		"ENCODER_DISCORD_AUDIO_TOKEN",
		"YOUTUBE_STREAM_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_DRIVE_FOLDER_ID",
		"AUTOSTREAM_DATA_DIR",
		"AUTOSTREAM_ARCHIVE_DIR",
		"FFMPEG_BIN",
		"TZ:",
	} {
		if strings.Contains(local, forbidden) {
			t.Errorf("local compose must not set legacy credential/config key %s", forbidden)
		}
	}
}

func TestProductionComposeReplacesBasePortAndStartsLoopbackRelay(t *testing.T) {
	body, err := os.ReadFile("docker-compose.prod.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(body)
	for _, required := range []string{
		"ports: !override",
		`- "127.0.0.1:8081:8080"`,
		"AUTOSTREAM_ENV: production",
		"AUTOSTREAM_OUTPUT_RELAY_URL: rtmp://127.0.0.1/autostream/{stream_id}",
		`network_mode: "service:encoder-recorder"`,
		"encoder-archives:/var/lib/autostream/archives",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("production compose is missing %q", required)
		}
	}
}

func activeEnvKeys(body string) map[string]bool {
	keys := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if ok {
			keys[strings.TrimSpace(key)] = true
		}
	}
	return keys
}
