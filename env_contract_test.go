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
		"AUTOSTREAM_NODE_CONFIG":                     true,
		"AUTOSTREAM_ENV":                             true,
		"AUTOSTREAM_CONFIG_REVISION":                 true,
		"AUTOSTREAM_BIND_ADDR":                       true,
		"AUTOSTREAM_OUTPUT_RELAY_URL":                true,
		"AUTOSTREAM_OUTPUT_RELAY_MODE":               true,
		"AUTOSTREAM_OUTPUT_RELAY_BINDING_ID":         true,
		"AUTOSTREAM_ENCODER_RECORDER_PORT":           true,
		"AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT": true,
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
	compose := strings.ReplaceAll(string(body), "\r\n", "\n")
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION: ${AUTOSTREAM_CONFIG_REVISION:-1}",
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:${AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT:-8080}",
		`127.0.0.1:${AUTOSTREAM_ENCODER_RECORDER_PORT:-8081}:${AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT:-8080}`,
		"./config:/etc/autostream-encoder-recorder",
	} {
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
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION: ${AUTOSTREAM_CONFIG_REVISION:-1}",
		"AUTOSTREAM_ENV: development",
		"AUTOSTREAM_BIND_ADDR: 0.0.0.0:${AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT:-8080}",
		`127.0.0.1:${AUTOSTREAM_ENCODER_RECORDER_PORT:-8081}:${AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT:-8080}`,
		`AUTOSTREAM_OUTPUT_RELAY_URL: ""`,
		"AUTOSTREAM_OUTPUT_RELAY_MODE: direct",
	} {
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

func TestProductionComposeDefaultsToDirectOutputAndKeepsRelayOptIn(t *testing.T) {
	body, err := os.ReadFile("docker-compose.prod.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := strings.ReplaceAll(string(body), "\r\n", "\n")
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION: ${AUTOSTREAM_CONFIG_REVISION:-1}",
		"ports: !override",
		`127.0.0.1:${AUTOSTREAM_ENCODER_RECORDER_PORT:-8081}:${AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT:-8080}`,
		"AUTOSTREAM_ENV: production",
		"AUTOSTREAM_OUTPUT_RELAY_URL: ${AUTOSTREAM_OUTPUT_RELAY_URL:-}",
		"AUTOSTREAM_OUTPUT_RELAY_MODE: ${AUTOSTREAM_OUTPUT_RELAY_MODE:-direct}",
		"AUTOSTREAM_REQUIRE_OUTPUT_RELAY: ${AUTOSTREAM_REQUIRE_OUTPUT_RELAY:-false}",
		"AUTOSTREAM_OUTPUT_RELAY_BINDING_ID: ${AUTOSTREAM_OUTPUT_RELAY_BINDING_ID:-}",
		"\n    depends_on:\n      output-relay:\n        condition: service_started\n",
		"./config:/etc/autostream-encoder-recorder:ro",
		"encoder-archives:/var/lib/autostream/archives",
		"\n  output-relay:\n",
		"image: tiangolo/nginx-rtmp:latest",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("production compose is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"network_mode:",
		"\n    networks:",
		"AUTOSTREAM_OUTPUT_RELAY_URL: rtmp://127.0.0.1",
		"./config:/etc/autostream-encoder-recorder\n",
		"AUTOSTREAM_OUTPUT_RELAY_MODE: live_api_static",
		"AUTOSTREAM_OUTPUT_RELAY_BINDING_ID: ${AUTOSTREAM_OUTPUT_RELAY_BINDING_ID:?",
	} {
		if strings.Contains(compose, forbidden) {
			t.Errorf("production compose must not contain %q", forbidden)
		}
	}
}

func TestStaticRelayBindingIDIsNonSecretAndMatchesControlPanelPolicy(t *testing.T) {
	env, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "AUTOSTREAM_OUTPUT_RELAY_BINDING_ID=relay-00000000-0000-0000-0000-000000000000") {
		t.Fatal(".env.example must expose a non-secret static relay binding ID placeholder")
	}
	for _, required := range []string{
		"AUTOSTREAM_OUTPUT_RELAY_MODE=direct",
		"legacy_stream_key",
		"AUTOSTREAM_OUTPUT_RELAY_MODE=live_api_static",
		"relay_binding_id",
	} {
		if !strings.Contains(string(env), required) {
			t.Errorf(".env.example is missing Relay mode compatibility guidance %q", required)
		}
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"AUTOSTREAM_OUTPUT_RELAY_BINDING_ID",
		"AUTOSTREAM_REQUIRE_OUTPUT_RELAY=true",
		"unavailable/fail-closed",
		"relay_binding_id",
		"relay-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
		"non-secret",
	} {
		if !strings.Contains(string(readme), required) {
			t.Errorf("README is missing static relay binding guidance %q", required)
		}
	}
}

func TestHostBindContractIsConfigurableAndUnprivileged(t *testing.T) {
	env, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	body := string(env)
	if !strings.Contains(body, "AUTOSTREAM_BIND_ADDR=127.0.0.1:8081") {
		t.Error(".env.example must retain the host default on port 8081")
	}
	for _, required := range []string{
		"AUTOSTREAM_ENCODER_RECORDER_PORT=8081",
		"AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT=8080",
	} {
		if !strings.Contains(body, required) {
			t.Errorf(".env.example is missing Docker port default %q", required)
		}
	}
	if !strings.Contains(body, "AUTOSTREAM_CONFIG_REVISION=1") {
		t.Error(".env.example must retain configuration revision 1 as the compatibility default")
	}
	if !strings.Contains(strings.ToLower(body), "root-owned") {
		t.Error(".env.example must document the root-owned updater probe config revision")
	}
	if !strings.Contains(body, "1024") || !strings.Contains(body, "65535") {
		t.Error(".env.example must document the supported unprivileged port range")
	}
	if !strings.Contains(body, "legacy 127.0.0.1:8080 fallback") {
		t.Error(".env.example must document the env-unset legacy port fallback")
	}

	unit, err := os.ReadFile("systemd/autostream-encoder-recorder.service.example")
	if err != nil {
		t.Fatal(err)
	}
	unitBody := string(unit)
	primaryEnv := "EnvironmentFile=/etc/autostream/encoder-recorder.env"
	managedEnv := "EnvironmentFile=-/opt/autostream/local-executor/ports/encoder-recorder.env"
	if !strings.Contains(unitBody, primaryEnv) {
		t.Error("systemd unit must load the configurable bind address from encoder-recorder.env")
	}
	if !strings.Contains(unitBody, managedEnv) {
		t.Error("systemd unit must optionally load the Control Panel managed port sidecar")
	}
	if strings.Index(unitBody, managedEnv) <= strings.Index(unitBody, primaryEnv) {
		t.Error("managed port sidecar must load after encoder-recorder.env so its bind address and revision win")
	}
	if !strings.Contains(unitBody, "AUTOSTREAM_CONFIG_REVISION") {
		t.Error("systemd unit must document the required configuration revision environment value")
	}
	if !strings.Contains(unitBody, "root-owned") {
		t.Error("systemd unit must document root ownership of the revision environment file")
	}
	if strings.Contains(unitBody, "8081") {
		t.Error("systemd unit must not hard-code the Encoder/Recorder port")
	}

	install, err := os.ReadFile("release/README.install.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION=1",
		"version, service_id, service_type, and config_revision",
		`PROBE_HOST="${PROBE_HOST:-127.0.0.1}"`,
		"PROBE_HOST='[::1]'",
	} {
		if !strings.Contains(string(install), required) {
			t.Errorf("release install guide is missing %q", required)
		}
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"AUTOSTREAM_CONFIG_REVISION=1",
		"increment it after a configuration change",
		"host/reverse-proxy responsibility",
		"`1024` through `65535`",
		"The production health authority is the host Local Executor.",
		"intentionally omit an in-container `healthcheck`",
		"does not add or repurpose `curl`, `wget`, or another unrelated executable",
		"probes the loopback published port for both `/health` and `/updater/version`",
		"the published port is the health port",
	} {
		if !strings.Contains(string(readme), required) {
			t.Errorf("README is missing %q", required)
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
