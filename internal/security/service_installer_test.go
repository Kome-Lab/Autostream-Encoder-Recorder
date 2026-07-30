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
		"root anchor directory has unsafe write or special mode bits",
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
		"AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS",
		"autostream-encoder-recorder-installer-test-scratch /mnt",
		"mount --rbind /usr /mnt/usr-lower",
		"mount --make-rprivate /mnt/usr-lower",
		"mount --rbind /etc /mnt/etc-lower",
		"mount --make-rprivate /mnt/etc-lower",
		"mount --rbind /var /mnt/var-lower",
		"mount --make-rprivate /mnt/var-lower",
		"mount --rbind /run /mnt/run-lower",
		"mount --make-rprivate /mnt/run-lower",
		"install -d -o root -g root -m 0755 /mnt/usr-upper",
		"install -d -o root -g root -m 0755 /mnt/usr-upper/local",
		"/mnt/var-upper/lib",
		"/mnt/var-upper/backups",
		"install -d -o root -g root -m 1777 /mnt/var-upper/tmp",
		"/mnt/usr-work",
		"-o nodev,nosuid,lowerdir=/mnt/usr-lower,upperdir=/mnt/usr-upper,workdir=/mnt/usr-work",
		"-o nodev,nosuid,lowerdir=/mnt/etc-lower,upperdir=/mnt/etc-upper,workdir=/mnt/etc-work",
		"-o nodev,nosuid,lowerdir=/mnt/var-lower,upperdir=/mnt/var-upper,workdir=/mnt/var-work",
		"-o nodev,nosuid,lowerdir=/mnt/run-lower,upperdir=/mnt/run-upper,workdir=/mnt/run-work",
		"autostream-encoder-recorder-installer-test-usr-overlay /usr",
		"autostream-encoder-recorder-installer-test-etc-overlay /etc",
		"autostream-encoder-recorder-installer-test-var-overlay /var",
		"autostream-encoder-recorder-installer-test-run-overlay /run",
		`host_run_systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"`,
		"mount --rbind /mnt/run-lower/systemd /run/systemd",
		"mount --make-rprivate /run/systemd",
		`awk '$5 == "/run/systemd" { found=1 } END { exit found ? 0 : 1 }'`,
		"host-backed /run/systemd bind mount is missing",
		"host-backed /run/systemd bind mount is not writable",
		"host-backed /run/systemd mount does not match its lower source",
		"autostream-encoder-recorder-installer-test-bin /usr/local/bin",
		"autostream-encoder-recorder-installer-test-opt /opt",
		"mount -t tmpfs -o ro,nodev,nosuid,noexec,mode=0555,uid=0,gid=0",
		"autostream-encoder-recorder-installer-test-sealed-mnt /mnt",
		`AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_RUN_SYSTEMD_ID="${host_run_systemd_identity}"`,
		"assert_sealed_scratch_mount()",
		"effective /mnt is not the read-only sealed fixture mount",
		"effective /mnt seal has unsafe metadata",
		"effective /mnt seal unexpectedly accepted a write",
		"isolated /usr overlay mount is missing",
		"isolated /etc overlay mount is missing",
		"isolated /var overlay mount is missing",
		"isolated /run overlay mount is missing",
		"isolated /usr/local/bin mount is missing",
		"isolated /opt mount is missing",
		"could not create an isolated sealed /mnt fixture",
		"could not create an isolated safe /usr fixture",
		"could not create an isolated safe /etc fixture",
		"could not create an isolated safe /etc/systemd fixture",
		"could not create an isolated safe /etc/systemd/system fixture",
		"could not create an isolated safe /usr/local fixture",
		"could not create an isolated safe /usr/local/bin fixture",
		"could not create an isolated safe /opt fixture",
		"could not create an isolated safe /var fixture",
		"could not create an isolated safe /var/lib fixture",
		"could not create an isolated safe /var/backups fixture",
		"could not create an isolated safe /var/tmp fixture",
		"could not create an isolated safe /run fixture",
		`legacy_unit_file_state="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"`,
		"legacy fixture must begin disabled",
		`readonly RUNTIME_UNIT_DIR="/run/systemd/system"`,
		`readonly RUNTIME_UNIT_PATH="${RUNTIME_UNIT_DIR}/${UNIT}"`,
		"systemd runtime unit directory is unsafe",
		`"${TARGET_LOCK}"; do`,
		"runner is not clean at",
		"unit_path_owned=false",
		"runtime_unit_path_owned=false",
		`runtime_unit_identity=""`,
		"runtime_unit_identity_matches=false",
		"runtime_unit_temp_owned=false",
		"target_lock_owned=false",
		"public_binary_owned=false",
		"public_alias_owned=false",
		"env_path_owned=false",
		"config_dir_owned=false",
		"state_dir_owned=false",
		"archive_dir_owned=false",
		"managed_root_owned=false",
		"install_backup_root_owned=false",
		"service_start_attempted=false",
		`old_pid_start_time=""`,
		"read_proc_pid_start_time()",
		`stat_tail="${stat_line##*) }"`,
		`start_time="${20}"`,
		"unit_enable_cleanup_needed=false",
		`if [[ ${service_start_attempted} == true &&`,
		`if [[ ${unit_enable_cleanup_needed} == true &&`,
		`if [[ ${runtime_unit_path_owned} == true &&`,
		`if [[ ${unit_path_owned} == true ]]; then`,
		`if [[ ${target_lock_owned} == true ]]; then`,
		`ln -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"`,
		`mv -Tf -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"`,
		"legacy_runtime_unit_before=",
		"legacy_fragment_before=",
		"legacy_exec_start_before=",
		"legacy_user_before=",
		"assert_legacy_pid1_state",
		"sync_private_unit_to_runtime",
		"assert_migrated_pid1_state",
		"assert_owned_runtime_unit_identity",
		"runtime unit identity changed",
		`systemctl show --property FragmentPath --value "${UNIT}"`,
		`systemctl show --property ExecStart --value "${UNIT}"`,
		`systemctl show --property User --value "${UNIT}"`,
		"AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_PREFLIGHT_PROBE",
		"preflight ownership probe unexpectedly passed",
		"preflight conflict probe unexpectedly succeeded",
		"preflight conflict changed the runtime sentinel inode",
		"preflight conflict changed the runtime sentinel hash",
		"preflight conflict changed the runtime sentinel FragmentPath",
		"preflight conflict changed the runtime sentinel ExecStart",
		"preflight conflict changed the runtime sentinel User",
		"preflight conflict changed the runtime sentinel PID",
		"preflight conflict changed the runtime sentinel enabled state",
		"idempotent reinstall changed the managed runtime unit inode",
		"idempotent reinstall changed the managed runtime unit hash",
		`old_pid_start_time="$(read_proc_pid_start_time "${old_pid}")"`,
		`current_pid_start_time="$(read_proc_pid_start_time "${old_pid}" 2>/dev/null || true)"`,
		"cleanup fallback refused a reused PID",
		"local cleanup_failed=false",
		"local cleanup_expected_unit_absent=false",
		"cleanup could not prove runtime unit ownership",
		"cleanup failed to stop ${UNIT}",
		"cleanup failed to remove ${RUNTIME_UNIT_PATH}",
		"cleanup daemon-reload failed",
		"cleanup left ${UNIT} active",
		"cleanup left ${UNIT} loaded",
		`cleanup_load_state="$(systemctl show --property LoadState --value "${UNIT}" 2>/dev/null || true)"`,
		`cleanup_fragment_path="$(systemctl show --property FragmentPath --value "${UNIT}" 2>/dev/null || true)"`,
		`if [[ ${cleanup_failed} == true && ${exit_code} -eq 0 ]]; then`,
		`runtime_sync_precommit_hook=""`,
		`cleanup_runtime_pre_remove_hook=""`,
		"runtime_unit_identity_is_owned()",
		"replace_runtime_unit_for_precommit_probe()",
		"replace_runtime_unit_for_cleanup_probe()",
		"restore_runtime_sync_race()",
		"runtime_race_active=false",
		`"${runtime_sync_precommit_hook}"`,
		"return 75",
		"runtime precommit race unexpectedly committed",
		"precommit race changed the foreign runtime unit inode",
		"precommit race changed the foreign runtime unit hash",
		"precommit race changed PID1 FragmentPath",
		"precommit race changed PID1 ExecStart",
		"precommit race changed PID1 User",
		"precommit race changed PID1 MainPID",
		"AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_PROBE",
		"cleanup runtime race hook failed",
		"cleanup runtime race did not promote a successful exit to failure",
		"cleanup runtime race removed or replaced the foreign inode",
		"cleanup runtime race changed the foreign runtime unit hash",
		"cleanup runtime race recovery did not restore the owned inode",
		"Keep cleanup race enablement semantics equivalent to the foreign probe unit.",
	} {
		if !strings.Contains(integration, marker) {
			t.Fatalf("installer integration fixture is missing scenario marker %q", marker)
		}
	}
	namespaceIndex := strings.Index(
		integration,
		`if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS:-} != "1" ]]; then`,
	)
	outerStrictIndex := strings.Index(
		integration,
		"exec unshare --mount --propagation private bash -c '\n    set -euo pipefail",
	)
	workDirIndex := strings.Index(integration, `WORK_DIR="$(mktemp`)
	if namespaceIndex < 0 ||
		outerStrictIndex <= namespaceIndex ||
		workDirIndex <= outerStrictIndex {
		t.Fatal("installer integration fixture must enter its isolated mount namespace before creating mutable state")
	}
	scratchIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-scratch /mnt")
	lowerIndex := strings.Index(integration, "mount --rbind /usr /mnt/usr-lower")
	privateIndex := strings.Index(integration, "mount --make-rprivate /mnt/usr-lower")
	etcLowerIndex := strings.Index(integration, "mount --rbind /etc /mnt/etc-lower")
	etcPrivateIndex := strings.Index(integration, "mount --make-rprivate /mnt/etc-lower")
	varLowerIndex := strings.Index(integration, "mount --rbind /var /mnt/var-lower")
	varPrivateIndex := strings.Index(integration, "mount --make-rprivate /mnt/var-lower")
	runLowerIndex := strings.Index(integration, "mount --rbind /run /mnt/run-lower")
	runPrivateIndex := strings.Index(integration, "mount --make-rprivate /mnt/run-lower")
	upperIndex := strings.Index(integration, "install -d -o root -g root -m 0755 /mnt/usr-upper")
	upperLocalIndex := strings.Index(integration, "install -d -o root -g root -m 0755 /mnt/usr-upper/local")
	workIndex := strings.Index(integration, "/mnt/usr-work")
	varWorkIndex := strings.Index(integration, "/mnt/var-work")
	overlayIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-usr-overlay /usr")
	etcOverlayIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-etc-overlay /etc")
	varOverlayIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-var-overlay /var")
	runOverlayIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-run-overlay /run")
	hostSystemdIdentityIndex := strings.Index(
		integration,
		`host_run_systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"`,
	)
	systemdBindIndex := strings.Index(integration, "mount --rbind /mnt/run-lower/systemd /run/systemd")
	binIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-bin /usr/local/bin")
	optIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-opt /opt")
	sealIndex := strings.Index(integration, "autostream-encoder-recorder-installer-test-sealed-mnt /mnt")
	if scratchIndex <= outerStrictIndex ||
		lowerIndex <= scratchIndex ||
		privateIndex <= lowerIndex ||
		etcLowerIndex <= privateIndex ||
		etcPrivateIndex <= etcLowerIndex ||
		varLowerIndex <= etcPrivateIndex ||
		varPrivateIndex <= varLowerIndex ||
		runLowerIndex <= varPrivateIndex ||
		runPrivateIndex <= runLowerIndex ||
		upperIndex <= runPrivateIndex ||
		upperLocalIndex <= upperIndex ||
		workIndex <= upperLocalIndex ||
		varWorkIndex <= workIndex ||
		overlayIndex <= varWorkIndex ||
		etcOverlayIndex <= overlayIndex ||
		varOverlayIndex <= etcOverlayIndex ||
		runOverlayIndex <= varOverlayIndex ||
		hostSystemdIdentityIndex <= runOverlayIndex ||
		systemdBindIndex <= hostSystemdIdentityIndex ||
		binIndex <= systemdBindIndex ||
		optIndex <= systemdBindIndex ||
		sealIndex <= binIndex ||
		sealIndex <= optIndex {
		t.Fatal("installer integration fixture must isolate /usr, /var, and /run before mounting host-backed or child fixture filesystems")
	}
	childExecIndex := strings.Index(
		integration,
		`AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_RUN_SYSTEMD_ID="${host_run_systemd_identity}"`,
	)
	sealAssertionIndex := strings.Index(integration, "\nassert_sealed_scratch_mount\n")
	runtimeSafetyIndex := strings.Index(integration, "systemd runtime unit directory is unsafe")
	if childExecIndex <= sealIndex ||
		sealAssertionIndex <= childExecIndex ||
		runtimeSafetyIndex <= sealAssertionIndex {
		t.Fatal("fixture must seal scratch aliases before restoring only the host-backed /run/systemd subtree")
	}
	cleanupStart := strings.Index(integration, "cleanup() {")
	if cleanupStart < 0 {
		t.Fatal("installer integration fixture cleanup is missing")
	}
	cleanupEnd := strings.Index(integration[cleanupStart:], "\n}\ntrap cleanup EXIT")
	trapIndex := strings.Index(integration, "trap cleanup EXIT")
	preflightStart := strings.Index(integration, "for path in")
	firstInstallerRun := strings.Index(integration, `"${EXTRACTED_ROOT}/install-autostream-encoder-recorder"`)
	if cleanupStart < 0 || cleanupEnd < 0 || preflightStart <= cleanupStart || firstInstallerRun <= preflightStart {
		t.Fatal("installer integration fixture cleanup/preflight structure is missing")
	}
	cleanup := integration[cleanupStart : cleanupStart+cleanupEnd]
	identityGateIndex := strings.Index(
		cleanup,
		`if [[ ${runtime_unit_path_owned} == true &&`+"\n"+`    -n ${runtime_unit_identity} &&`,
	)
	serviceGateIndex := strings.Index(
		cleanup,
		`if [[ ${service_start_attempted} == true &&`+"\n"+`    ${runtime_unit_identity_matches} == true ]]; then`,
	)
	serviceStopIndex := strings.Index(cleanup, `systemctl stop "${UNIT}"`)
	fallbackIdentityIndex := strings.Index(
		cleanup,
		`current_pid_start_time="$(read_proc_pid_start_time "${old_pid}" 2>/dev/null || true)"`,
	)
	fallbackKillIndex := strings.Index(cleanup, `kill "${old_pid}"`)
	cleanupRaceHookIndex := strings.Index(
		cleanup,
		`"${cleanup_runtime_pre_remove_hook}"`,
	)
	runtimeRemovalGateOffset := -1
	if serviceStopIndex >= 0 {
		runtimeRemovalGateOffset = strings.Index(
			cleanup[serviceStopIndex:],
			`if [[ ${runtime_unit_path_owned} == true &&`,
		)
	}
	runtimeRemoveIndex := strings.Index(cleanup, `rm -f -- "${RUNTIME_UNIT_PATH}"`)
	preRemoveIdentityIndex := -1
	if runtimeRemoveIndex >= 0 {
		preRemoveIdentityIndex = strings.LastIndex(
			cleanup[:runtimeRemoveIndex],
			"if ! runtime_unit_identity_is_owned; then",
		)
	}
	unitGateIndex := strings.Index(cleanup, `if [[ ${unit_path_owned} == true ]]; then`)
	unitRemoveIndex := strings.Index(cleanup, `rm -f -- "${UNIT_PATH}"`)
	cleanupReloadIndex := strings.Index(cleanup, `if ! systemctl daemon-reload`)
	finalInactiveIndex := strings.Index(cleanup, `systemctl is-active --quiet "${UNIT}"`)
	finalLoadIndex := strings.Index(
		cleanup,
		`cleanup_load_state="$(systemctl show --property LoadState --value "${UNIT}" 2>/dev/null || true)"`,
	)
	exitPromotionIndex := strings.Index(
		cleanup,
		`if [[ ${cleanup_failed} == true && ${exit_code} -eq 0 ]]; then`,
	)
	cleanupExitIndex := strings.LastIndex(cleanup, `exit "${exit_code}"`)
	if identityGateIndex < 0 ||
		serviceGateIndex <= identityGateIndex ||
		serviceStopIndex <= serviceGateIndex ||
		fallbackIdentityIndex <= serviceStopIndex ||
		fallbackKillIndex <= fallbackIdentityIndex ||
		runtimeRemovalGateOffset < 0 ||
		runtimeRemoveIndex <= serviceStopIndex+runtimeRemovalGateOffset ||
		cleanupRaceHookIndex <= fallbackKillIndex ||
		preRemoveIdentityIndex <= cleanupRaceHookIndex ||
		unitGateIndex <= runtimeRemoveIndex ||
		unitRemoveIndex <= unitGateIndex ||
		cleanupReloadIndex <= runtimeRemoveIndex ||
		finalInactiveIndex <= cleanupReloadIndex || finalLoadIndex <= finalInactiveIndex ||
		exitPromotionIndex <= finalLoadIndex || cleanupExitIndex <= exitPromotionIndex {
		t.Fatal("fixture cleanup must gate service stop and runtime removal on strict device/inode ownership")
	}
	for _, unguarded := range []string{
		"set +e\n  systemctl stop",
		`systemctl stop "${UNIT}" >/dev/null 2>&1
  systemctl disable "${UNIT}"`,
		`rm -f -- "${UNIT_PATH}" "${RUNTIME_UNIT_PATH}"`,
		`rm -f -- "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}" "${ENV_PATH}" "${TARGET_LOCK}"`,
	} {
		if strings.Contains(cleanup, unguarded) {
			t.Fatalf("installer integration fixture cleanup retains unguarded host mutation %q", unguarded)
		}
	}
	targetLockPreflightIndex := strings.Index(
		integration[preflightStart:firstInstallerRun],
		`"${TARGET_LOCK}"`,
	)
	if targetLockPreflightIndex < 0 {
		t.Fatal("installer integration fixture must reject a pre-existing updater target lock before mutation")
	}
	legacyRuntimeStart := strings.Index(integration, "create_legacy_runtime_unit() {")
	if legacyRuntimeStart < 0 {
		t.Fatal("installer integration fixture legacy runtime unit helper is missing")
	}
	legacyRuntimeEnd := strings.Index(integration[legacyRuntimeStart:], "\n}\n\nsync_private_unit_to_runtime()")
	if legacyRuntimeEnd < 0 {
		t.Fatal("installer integration fixture legacy runtime unit helper is unterminated")
	}
	legacyRuntime := integration[legacyRuntimeStart : legacyRuntimeStart+legacyRuntimeEnd]
	for _, marker := range []string{
		`mktemp "${RUNTIME_UNIT_DIR}/.${UNIT}.fixture.XXXXXXXX"`,
		`install -o root -g root -m 0644 "${UNIT_PATH}" "${RUNTIME_UNIT_TEMP}"`,
		`ln -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"`,
		"runtime_unit_path_owned=true",
		`runtime_unit_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"`,
		`sync -f "${RUNTIME_UNIT_DIR}"`,
		"assert_owned_runtime_unit_identity",
	} {
		if !strings.Contains(legacyRuntime, marker) {
			t.Fatalf("legacy runtime unit creation is missing atomic no-clobber marker %q", marker)
		}
	}
	runtimeSyncStart := strings.Index(integration, "sync_private_unit_to_runtime() {")
	if runtimeSyncStart < 0 {
		t.Fatal("installer integration fixture runtime synchronization helper is missing")
	}
	runtimeSyncEnd := strings.Index(integration[runtimeSyncStart:], "\n}\n\nassert_legacy_pid1_state()")
	if runtimeSyncEnd < 0 {
		t.Fatal("installer integration fixture runtime synchronization helper is unterminated")
	}
	runtimeSync := integration[runtimeSyncStart : runtimeSyncStart+runtimeSyncEnd]
	for _, marker := range []string{
		`mktemp "${RUNTIME_UNIT_DIR}/.${UNIT}.fixture.XXXXXXXX"`,
		`install -o root -g root -m 0644 "${UNIT_PATH}" "${RUNTIME_UNIT_TEMP}"`,
		`mv -Tf -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"`,
		`runtime_unit_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"`,
		`sync -f "${RUNTIME_UNIT_DIR}"`,
		"assert_owned_runtime_unit_identity",
		"systemctl daemon-reload",
	} {
		if !strings.Contains(runtimeSync, marker) {
			t.Fatalf("runtime unit synchronization is missing atomic replacement marker %q", marker)
		}
	}
	runtimeMoveIndex := strings.Index(runtimeSync, `mv -Tf -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"`)
	precommitIdentityIndex := -1
	if runtimeMoveIndex >= 0 {
		precommitIdentityIndex = strings.LastIndex(
			runtimeSync[:runtimeMoveIndex],
			"runtime_unit_identity_is_owned",
		)
	}
	stageSyncIndex := strings.Index(runtimeSync, `sync -f "${RUNTIME_UNIT_TEMP}"`)
	if count := strings.Count(runtimeSync, "assert_owned_runtime_unit_identity"); count != 2 ||
		stageSyncIndex < 0 || precommitIdentityIndex <= stageSyncIndex ||
		runtimeMoveIndex <= precommitIdentityIndex {
		t.Fatal("runtime synchronization must revalidate the owned inode immediately before commit")
	}
	if count := strings.Count(integration, `assert_legacy_pid1_state "`); count < 3 {
		t.Fatalf("fixture must assert the legacy runtime/PID1 state for both rollback faults, got %d checks", count)
	}
	legacyPID1StateStart := strings.Index(integration, "assert_legacy_pid1_state() {")
	if legacyPID1StateStart < 0 {
		t.Fatal("fixture is missing the legacy PID1 state assertion helper")
	}
	legacyPID1StateEnd := strings.Index(
		integration[legacyPID1StateStart:],
		"\n}\n\nassert_migrated_pid1_state()",
	)
	if legacyPID1StateEnd < 0 {
		t.Fatal("legacy PID1 state assertion helper is unterminated")
	}
	legacyPID1State := integration[legacyPID1StateStart : legacyPID1StateStart+legacyPID1StateEnd]
	for _, marker := range []string{
		`${exec_start_now} == *"path=/usr/bin/sleep"*`,
		`${exec_start_now} == *"argv[]=/usr/bin/sleep infinity"*`,
		`pid_start_time_now="$(read_proc_pid_start_time "${old_pid}")"`,
		`[[ ${pid_start_time_now} == "${old_pid_start_time}" ]]`,
	} {
		if !strings.Contains(legacyPID1State, marker) {
			t.Fatalf("legacy PID1 state assertion must compare stable command/process semantics: missing %q", marker)
		}
	}
	if strings.Contains(
		legacyPID1State,
		`[[ ${exec_start_now} == "${legacy_exec_start_before}" ]]`,
	) {
		t.Fatal("legacy PID1 state assertion must not compare volatile ExecStart runtime metadata")
	}
	if count := strings.Count(integration, `assert_migrated_pid1_state "`); count < 2 {
		t.Fatalf("fixture must assert migrated PID1 state after migration and idempotent reinstall, got %d checks", count)
	}
	if count := strings.Count(
		integration,
		`old_pid_start_time="$(read_proc_pid_start_time "${old_pid}")"`,
	); count != 2 {
		t.Fatalf("fixture must record PID start time after both fixture service starts, got %d", count)
	}
	probeGateIndex := strings.Index(
		integration,
		`if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_PREFLIGHT_PROBE:-} == "1" ]]; then`,
	)
	preflightCompleteIndex := strings.Index(integration, "preflight_complete=true")
	sentinelIndex := strings.Index(integration, "runtime_sentinel_identity_before=")
	probeInvocationIndex := strings.Index(
		integration,
		"AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_PREFLIGHT_PROBE=1 bash",
	)
	cleanupRaceProbeBranchIndex := strings.Index(
		integration,
		`if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_PROBE:-} == "1" ]]; then`,
	)
	cleanupRaceInvocationIndex := strings.Index(
		integration,
		"AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_PROBE=1",
	)
	sentinelCleanupIndex := strings.Index(
		integration,
		"runtime_unit_path_owned=false\nruntime_unit_identity=\"\"\nold_pid=\"\"\nold_pid_start_time=\"\"",
	)
	if cleanupRaceProbeBranchIndex <= trapIndex ||
		cleanupRaceProbeBranchIndex >= preflightStart ||
		probeGateIndex <= preflightStart ||
		preflightCompleteIndex <= probeGateIndex ||
		sentinelIndex <= preflightCompleteIndex ||
		cleanupRaceInvocationIndex <= sentinelIndex ||
		probeInvocationIndex <= cleanupRaceInvocationIndex ||
		sentinelCleanupIndex <= probeInvocationIndex {
		t.Fatal("fixture must prove a nested preflight conflict cannot mutate its running runtime sentinel")
	}
	migrationIndex := strings.Index(
		integration,
		`"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/migration.out"`,
	)
	idempotentIndex := strings.Index(
		integration,
		`"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/idempotent.out"`,
	)
	idempotentBaselineIndex := strings.Index(integration, "idempotent_runtime_identity_before=")
	raceBaselineIndex := strings.Index(integration, "runtime_race_fragment_before=")
	raceRestoreIndex := strings.Index(integration, "restore_runtime_sync_race ||")
	malformedIndex := strings.Index(
		integration,
		`"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \`+"\n"+`  > "${WORK_DIR}/malformed-current.out"`,
	)
	migrationSyncOffset := -1
	if migrationIndex >= 0 {
		migrationSyncOffset = strings.Index(integration[migrationIndex:], "\nsync_private_unit_to_runtime\n")
	}
	if migrationIndex < 0 || migrationSyncOffset < 0 ||
		raceBaselineIndex <= migrationIndex || raceRestoreIndex <= raceBaselineIndex ||
		idempotentBaselineIndex <= migrationIndex+migrationSyncOffset ||
		idempotentIndex <= idempotentBaselineIndex ||
		idempotentIndex <= migrationIndex+migrationSyncOffset ||
		malformedIndex <= idempotentIndex {
		t.Fatal("fixture must atomically synchronize the managed runtime unit after migration")
	}
	idempotentSection := integration[idempotentBaselineIndex:malformedIndex]
	if strings.Contains(idempotentSection, "\nsync_private_unit_to_runtime\n") {
		t.Fatal("idempotent reinstall must verify, not replace, the already-loaded runtime unit")
	}
	for _, marker := range []string{
		`assert_migrated_pid1_state "idempotent reinstall"`,
		"idempotent_runtime_identity_before=",
		"idempotent_runtime_hash_before=",
	} {
		if !strings.Contains(idempotentSection, marker) {
			t.Fatalf("idempotent runtime verification is missing %q", marker)
		}
	}
	if count := strings.Count(integration, "[Install]\nWantedBy=multi-user.target"); count != 2 {
		t.Fatalf("integration fixture must define two enable-capable but disabled units, got %d", count)
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
