package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncoderRecorderReleaseShipsManagedServiceInstaller(t *testing.T) {
	root := filepath.Join("..", "..")
	installerPath := filepath.Join(root, "release", "install-autostream-encoder-recorder")
	installerBytes, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)

	for _, marker := range []string{
		"set -euo pipefail",
		`readonly SERVICE_NAME="encoder-recorder"`,
		`readonly MANAGED_ROOT="/opt/autostream/encoder-recorder"`,
		`readonly PUBLIC_BINARY="/usr/local/bin/autostream-encoder-recorder"`,
		`readonly PUBLIC_ALIAS="/usr/local/bin/encoder-recorder"`,
		`readonly ENV_DEST="/etc/autostream/encoder-recorder.env"`,
		`readonly UNIT_DEST="/etc/systemd/system/autostream-encoder-recorder.service"`,
		`readonly BACKUP_ROOT="${BACKUP_BASE}/install-migrations"`,
		`readonly BACKUP_DIR="${BACKUP_ROOT}/encoder-recorder"`,
		"sha256sum --check --strict",
		"release-manifest.json",
		".artifact-sha256",
		".version",
		`flock -n 9`,
		`[[ ${version_first_line} == "autostream-encoder-recorder ${VERSION}" ]]`,
		`[[ ${managed_version_first_line} == "autostream-encoder-recorder ${VERSION}" ]]`,
		"root-only recovery evidence preserved at",
		"systemctl daemon-reload",
	} {
		if !strings.Contains(installer, marker) {
			t.Fatalf("service installer is missing %q", marker)
		}
	}

	workflowBytes, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release-host.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBytes)
	for _, marker := range []string{
		`run: bash -n release/install-autostream-encoder-recorder`,
		`run: sudo bash release/test-install-autostream-encoder-recorder-integration.sh`,
		`cp release/install-autostream-encoder-recorder "${root}/install-autostream-encoder-recorder"`,
		`chmod 0755 "${root}/install-autostream-encoder-recorder"`,
		`artifacts/autostream-encoder-recorder_${{ needs.release-host.outputs.version }}_linux_amd64.tar.gz`,
		`artifacts/autostream-encoder-recorder_${{ needs.release-host.outputs.version }}_linux_arm64.tar.gz`,
	} {
		if !strings.Contains(workflow, marker) {
			t.Fatalf("host release workflow is missing installer packaging marker %q", marker)
		}
	}

	ciBytes, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ciBytes), "run: bash -n release/install-autostream-encoder-recorder") {
		t.Fatal("CI must reject a syntactically invalid managed installer")
	}
	if !strings.Contains(string(ciBytes), "run: sudo bash release/test-install-autostream-encoder-recorder-integration.sh") {
		t.Fatal("CI must execute the production installer integration scenarios")
	}

	integrationBytes, err := os.ReadFile(filepath.Join(root, "release", "test-install-autostream-encoder-recorder-integration.sh"))
	if err != nil {
		t.Fatal(err)
	}
	integration := string(integrationBytes)
	for _, marker := range []string{
		"unshare --mount --propagation private",
		"root-only recovery evidence preserved at",
		"systemctl show --property MainPID",
		"config.yml",
		"idempotent reinstall",
		"managed current link must be owned by root:root",
		"another privileged update is already active",
	} {
		if !strings.Contains(integration, marker) {
			t.Fatalf("installer integration fixture is missing scenario marker %q", marker)
		}
	}

	unitBytes, err := os.ReadFile(filepath.Join(root, "systemd", "autostream-encoder-recorder.service.example"))
	if err != nil {
		t.Fatal(err)
	}
	unit := string(unitBytes)
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/autostream-encoder-recorder") {
		t.Fatal("Encoder Recorder systemd unit must use the stable public binary path")
	}
	if strings.Contains(unit, "ExecStart=/opt/autostream/encoder-recorder/current/") {
		t.Fatal("Encoder Recorder systemd unit exposes installer-owned release internals")
	}

	guideBytes, err := os.ReadFile(filepath.Join(root, "release", "README.install.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(guideBytes)
	for _, marker := range []string{
		"sudo install -d -o root -g root -m 0755 /opt/autostream/releases/artifacts",
		"gh attestation verify autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz",
		"sudo tar --no-same-owner --no-same-permissions -xzf autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz",
		"sudo ./install-autostream-encoder-recorder",
		"installer-owned",
	} {
		if !strings.Contains(guide, marker) {
			t.Fatalf("install guide is missing simple installer marker %q", marker)
		}
	}
}
