#!/bin/bash
set -euo pipefail

umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

die() {
  printf 'encoder-recorder installer integration test: %s\n' "$*" >&2
  exit 1
}

assert_not_enabled() {
  if systemctl is-enabled --quiet "${UNIT}"; then
    unit_enable_cleanup_needed=true
    die "installer unexpectedly enabled ${UNIT}"
  fi
  return 0
}

[[ ${EUID} -eq 0 ]] || die "must run as root"
[[ $(uname -m) == "x86_64" ]] || die "this integration fixture requires an amd64 Linux runner"

if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS:-} != "1" ]]; then
  exec unshare --mount --propagation private bash -c '
    set -euo pipefail
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-scratch /mnt
    install -d -o root -g root -m 0755 /mnt/usr-lower
    mount --rbind /usr /mnt/usr-lower
    mount --make-rprivate /mnt/usr-lower
    install -d -o root -g root -m 0755 /mnt/etc-lower
    mount --rbind /etc /mnt/etc-lower
    mount --make-rprivate /mnt/etc-lower
    install -d -o root -g root -m 0755 /mnt/var-lower
    mount --rbind /var /mnt/var-lower
    mount --make-rprivate /mnt/var-lower
    install -d -o root -g root -m 0755 /mnt/run-lower
    mount --rbind /run /mnt/run-lower
    mount --make-rprivate /mnt/run-lower
    install -d -o root -g root -m 0755 /mnt/usr-upper
    install -d -o root -g root -m 0755 /mnt/usr-upper/local
    install -d -o root -g root -m 0755 \
      /mnt/etc-upper \
      /mnt/etc-upper/systemd \
      /mnt/etc-upper/systemd/system \
      /mnt/var-upper \
      /mnt/var-upper/lib \
      /mnt/var-upper/backups \
      /mnt/run-upper
    install -d -o root -g root -m 1777 /mnt/var-upper/tmp
    install -d -o root -g root -m 0700 \
      /mnt/usr-work \
      /mnt/etc-work \
      /mnt/var-work \
      /mnt/run-work
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/usr-lower,upperdir=/mnt/usr-upper,workdir=/mnt/usr-work \
      autostream-encoder-recorder-installer-test-usr-overlay /usr
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/etc-lower,upperdir=/mnt/etc-upper,workdir=/mnt/etc-work \
      autostream-encoder-recorder-installer-test-etc-overlay /etc
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/var-lower,upperdir=/mnt/var-upper,workdir=/mnt/var-work \
      autostream-encoder-recorder-installer-test-var-overlay /var
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/run-lower,upperdir=/mnt/run-upper,workdir=/mnt/run-work \
      autostream-encoder-recorder-installer-test-run-overlay /run
    host_run_systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"
    mount --rbind /mnt/run-lower/systemd /run/systemd
    mount --make-rprivate /run/systemd
    [[ $(stat -c "%d:%i" -- /run/systemd) == "${host_run_systemd_identity}" ]]
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-bin /usr/local/bin
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-opt /opt
    mount -t tmpfs -o ro,nodev,nosuid,noexec,mode=0555,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-sealed-mnt /mnt
    exec env \
      AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS=1 \
      AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_RUN_SYSTEMD_ID="${host_run_systemd_identity}" \
      bash "$1"
  ' autostream-encoder-recorder-installer-test-mount "$0"
fi

assert_sealed_scratch_mount() {
  local probe="/mnt/.autostream-installer-write-probe"

  awk '
    $5 == "/mnt" {
      has_ro = 0
      has_nodev = 0
      has_nosuid = 0
      has_noexec = 0
      option_count = split($6, options, ",")
      for (option = 1; option <= option_count; option++) {
        has_ro = has_ro || options[option] == "ro"
        has_nodev = has_nodev || options[option] == "nodev"
        has_nosuid = has_nosuid || options[option] == "nosuid"
        has_noexec = has_noexec || options[option] == "noexec"
      }
      if (!has_ro || !has_nodev || !has_nosuid || !has_noexec) {
        next
      }
      for (field = 7; field <= NF; field++) {
        if ($field == "-" &&
            $(field + 1) == "tmpfs" &&
            $(field + 2) == "autostream-encoder-recorder-installer-test-sealed-mnt") {
          found = 1
        }
      }
    }
    END { exit found ? 0 : 1 }
  ' /proc/self/mountinfo || die "effective /mnt is not the read-only sealed fixture mount"
  [[ $(stat -f -c '%T' -- /mnt) == "tmpfs" ]] || \
    die "effective /mnt is not backed by the sealed tmpfs"
  [[ $(stat -c '%U:%G:%a' -- /mnt) == "root:root:555" ]] || \
    die "effective /mnt seal has unsafe metadata"
  if touch -- "${probe}" 2>/dev/null; then
    rm -f -- "${probe}"
    die "effective /mnt seal unexpectedly accepted a write"
  fi
}

assert_sealed_scratch_mount
grep -Eq ' /usr .* - overlay autostream-encoder-recorder-installer-test-usr-overlay ' \
  /proc/self/mountinfo || die "isolated /usr overlay mount is missing"
grep -Eq ' /etc .* - overlay autostream-encoder-recorder-installer-test-etc-overlay ' \
  /proc/self/mountinfo || die "isolated /etc overlay mount is missing"
grep -Eq ' /var .* - overlay autostream-encoder-recorder-installer-test-var-overlay ' \
  /proc/self/mountinfo || die "isolated /var overlay mount is missing"
grep -Eq ' /run .* - overlay autostream-encoder-recorder-installer-test-run-overlay ' \
  /proc/self/mountinfo || die "isolated /run overlay mount is missing"
awk '$5 == "/run/systemd" { found=1 } END { exit found ? 0 : 1 }' \
  /proc/self/mountinfo || die "host-backed /run/systemd bind mount is missing"
[[ $(stat -c '%d:%i' -- /run/systemd) == \
  "${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_RUN_SYSTEMD_ID:-}" ]] || \
  die "host-backed /run/systemd mount does not match its lower source"
awk '$5 == "/run/systemd" && $6 ~ /^rw(,|$)/ { found=1 } END { exit found ? 0 : 1 }' \
  /proc/self/mountinfo || die "host-backed /run/systemd bind mount is not writable"
grep -Eq ' /usr/local/bin .* - tmpfs autostream-encoder-recorder-installer-test-bin ' \
  /proc/self/mountinfo || die "isolated /usr/local/bin mount is missing"
grep -Eq ' /opt .* - tmpfs autostream-encoder-recorder-installer-test-opt ' \
  /proc/self/mountinfo || die "isolated /opt mount is missing"
[[ $(stat -c '%U:%G:%a' -- /mnt) == "root:root:555" ]] || \
  die "could not create an isolated sealed /mnt fixture"
[[ $(stat -c '%U:%G:%a' -- /usr) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr fixture"
[[ $(stat -c '%U:%G:%a' -- /etc) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc fixture"
[[ $(stat -c '%U:%G:%a' -- /etc/systemd) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc/systemd fixture"
[[ $(stat -c '%U:%G:%a' -- /etc/systemd/system) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc/systemd/system fixture"
[[ $(stat -c '%U:%G:%a' -- /usr/local) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local fixture"
[[ $(stat -c '%U:%G:%a' -- /usr/local/bin) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local/bin fixture"
[[ $(stat -c '%U:%G:%a' -- /opt) == "root:root:755" ]] || \
  die "could not create an isolated safe /opt fixture"
[[ $(stat -c '%U:%G:%a' -- /var) == "root:root:755" ]] || \
  die "could not create an isolated safe /var fixture"
[[ $(stat -c '%U:%G:%a' -- /var/lib) == "root:root:755" ]] || \
  die "could not create an isolated safe /var/lib fixture"
[[ $(stat -c '%U:%G:%a' -- /var/backups) == "root:root:755" ]] || \
  die "could not create an isolated safe /var/backups fixture"
[[ $(stat -c '%U:%G:%a' -- /var/tmp) == "root:root:1777" ]] || \
  die "could not create an isolated safe /var/tmp fixture"
[[ $(stat -c '%U:%G:%a' -- /run) == "root:root:755" ]] || \
  die "could not create an isolated safe /run fixture"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
[[ ${SCRIPT_DIR} == /* && -d ${SCRIPT_DIR} ]] || die "could not resolve the fixture directory"
readonly SCRIPT_DIR
readonly INSTALLER_SOURCE="${SCRIPT_DIR}/install-autostream-encoder-recorder"
readonly VERSION="v9.9.9"
readonly FIXTURE_COMMIT="0000000000000000000000000000000000000000"
readonly FIXTURE_BUILD_DATE="2026-01-01T00:00:00Z"
readonly ARTIFACT_ID="autostream-encoder-recorder_${VERSION}_linux_amd64"
readonly UNIT="autostream-encoder-recorder.service"
readonly UNIT_PATH="/etc/systemd/system/${UNIT}"
readonly RUNTIME_UNIT_DIR="/run/systemd/system"
readonly RUNTIME_UNIT_PATH="${RUNTIME_UNIT_DIR}/${UNIT}"
[[ -d ${RUNTIME_UNIT_DIR} && ! -L ${RUNTIME_UNIT_DIR} &&
  $(readlink -f -- "${RUNTIME_UNIT_DIR}") == "${RUNTIME_UNIT_DIR}" &&
  $(stat -c '%U:%G:%a' -- "${RUNTIME_UNIT_DIR}") == "root:root:755" ]] || \
  die "systemd runtime unit directory is unsafe"
readonly PUBLIC_BINARY="/usr/local/bin/autostream-encoder-recorder"
readonly PUBLIC_ALIAS="/usr/local/bin/encoder-recorder"
readonly ENV_PATH="/etc/autostream/encoder-recorder.env"
readonly CONFIG_DIR="/etc/autostream-encoder-recorder"
readonly CONFIG_PATH="${CONFIG_DIR}/config.yml"
readonly STATE_DIR="/var/lib/autostream/encoder-recorder"
readonly ARCHIVE_DIR="/var/lib/autostream/archives"
readonly MANAGED_ROOT="/opt/autostream/encoder-recorder"
readonly INSTALL_BACKUP_ROOT="/var/backups/autostream/install-migrations/encoder-recorder"
TARGET_LOCK_ID="$(printf '%s' "${UNIT}" | sha256sum | awk 'NR == 1 { print substr($1, 1, 12) }')"
[[ ${TARGET_LOCK_ID} =~ ^[0-9a-f]{12}$ ]] || die "could not derive the updater target lock ID"
readonly TARGET_LOCK_ID
readonly TARGET_LOCK="/run/autostream-updater/.autostream-updater-${TARGET_LOCK_ID}.lock"
readonly SHARED_HOST_SETUP_LOCK="/run/autostream-updater/.autostream-runtime-host-setup.lock"
WORK_DIR="$(mktemp -d /var/tmp/autostream-encoder-recorder-installer-test.XXXXXXXX)"
[[ ${WORK_DIR} == /var/tmp/autostream-encoder-recorder-installer-test.* &&
  -d ${WORK_DIR} && ! -L ${WORK_DIR} &&
  $(readlink -f -- "${WORK_DIR}") == "${WORK_DIR}" &&
  $(stat -c '%U:%G:%a' -- "${WORK_DIR}") == "root:root:700" ]] || \
  die "could not create a safe fixture work directory"
readonly WORK_DIR
readonly ARTIFACTS_DIR="${WORK_DIR}/artifacts"
readonly EXTRACTED_ROOT="${ARTIFACTS_DIR}/${ARTIFACT_ID}"
readonly ARCHIVE="${ARTIFACTS_DIR}/${ARTIFACT_ID}.tar.gz"
readonly LEGACY_UNIT_CONTENT="encoder-recorder-installer-integration-legacy-unit"
readonly LEGACY_BINARY_CONTENT="encoder-recorder-installer-integration-legacy-binary"
readonly LEGACY_ALIAS_CONTENT="encoder-recorder-installer-integration-legacy-alias"
readonly LEGACY_ENV_CONTENT="ENCODER_RECORDER_INSTALLER_INTEGRATION_ENV=preserve-exactly"
readonly LEGACY_CONFIG_CONTENT="encoder-recorder-installer-integration-config-preserve-exactly"

work_dir_owned=true
preflight_complete=false
autostream_account_owned=false
unit_path_owned=false
runtime_unit_path_owned=false
runtime_unit_identity=""
runtime_unit_temp_owned=false
public_binary_owned=false
public_alias_owned=false
env_path_owned=false
config_dir_owned=false
state_dir_owned=false
archive_dir_owned=false
managed_root_owned=false
install_backup_root_owned=false
shared_host_setup_lock_owned=false
target_lock_owned=false
root_unpack_owned=false
recovery_path_owned=false
service_start_attempted=false
service_started_by_fixture=false
unit_enable_cleanup_needed=false
RUNTIME_UNIT_TEMP=""
RECOVERY_PATH=""
old_pid=""
old_pid_start_time=""
runtime_sync_precommit_hook=""
cleanup_runtime_pre_remove_hook=""
cleanup_runtime_race_report=""
runtime_race_active=false
runtime_race_backup=""
runtime_race_foreign_stage=""
runtime_race_foreign_identity=""
runtime_race_foreign_hash=""

adopt_installer_paths() {
  [[ ${preflight_complete} == true ]] || die "cannot adopt paths before preflight completes"
  [[ ! -e ${UNIT_PATH} && ! -L ${UNIT_PATH} ]] || unit_path_owned=true
  [[ ! -e ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} ]] || public_binary_owned=true
  [[ ! -e ${PUBLIC_ALIAS} && ! -L ${PUBLIC_ALIAS} ]] || public_alias_owned=true
  [[ ! -e ${ENV_PATH} && ! -L ${ENV_PATH} ]] || env_path_owned=true
  [[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || state_dir_owned=true
  [[ ! -e ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} ]] || archive_dir_owned=true
  [[ ! -e ${MANAGED_ROOT} && ! -L ${MANAGED_ROOT} ]] || managed_root_owned=true
  [[ ! -e ${INSTALL_BACKUP_ROOT} && ! -L ${INSTALL_BACKUP_ROOT} ]] || \
    install_backup_root_owned=true
  [[ ! -e ${SHARED_HOST_SETUP_LOCK} && ! -L ${SHARED_HOST_SETUP_LOCK} ]] || \
    shared_host_setup_lock_owned=true
  [[ ! -e ${TARGET_LOCK} && ! -L ${TARGET_LOCK} ]] || target_lock_owned=true
  [[ ! -e /unpack && ! -L /unpack ]] || root_unpack_owned=true
  if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
    autostream_account_owned=true
  fi
}

read_proc_pid_start_time() {
  local pid=$1
  local start_time
  local stat_line
  local stat_tail

  [[ ${pid} =~ ^[1-9][0-9]*$ && -r /proc/${pid}/stat ]] || return 1
  IFS= read -r stat_line < "/proc/${pid}/stat" || return 1
  [[ ${stat_line} == *") "* ]] || return 1
  stat_tail="${stat_line##*) }"
  set -- ${stat_tail}
  [[ $# -ge 20 ]] || return 1
  start_time="${20}"
  [[ ${start_time} =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${start_time}"
}

runtime_unit_identity_is_owned() {
  [[ ${runtime_unit_path_owned} == true &&
    -n ${runtime_unit_identity} &&
    -f ${RUNTIME_UNIT_PATH} &&
    ! -L ${RUNTIME_UNIT_PATH} &&
    $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == "${runtime_unit_identity}" ]]
}

restore_runtime_sync_race() {
  local current_identity=""

  [[ ${runtime_race_active} == true ]] || return 0
  [[ -n ${runtime_race_backup} &&
    -f ${runtime_race_backup} &&
    ! -L ${runtime_race_backup} &&
    $(stat -c '%d:%i' -- "${runtime_race_backup}") == "${runtime_unit_identity}" ]] || \
    return 1
  if [[ -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]]; then
    current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  fi
  if [[ ${current_identity} == "${runtime_race_foreign_identity}" ]]; then
    mv -Tf -- "${runtime_race_backup}" "${RUNTIME_UNIT_PATH}" || return 1
    runtime_race_backup=""
  elif [[ ${current_identity} == "${runtime_unit_identity}" ]]; then
    rm -f -- "${runtime_race_backup}" || return 1
    runtime_race_backup=""
  else
    return 1
  fi
  if [[ -n ${runtime_race_foreign_stage} ]]; then
    [[ -f ${runtime_race_foreign_stage} &&
      ! -L ${runtime_race_foreign_stage} &&
      $(stat -c '%d:%i' -- "${runtime_race_foreign_stage}") == \
        "${runtime_race_foreign_identity}" ]] || return 1
    rm -f -- "${runtime_race_foreign_stage}" || return 1
    runtime_race_foreign_stage=""
  fi
  sync -f "${RUNTIME_UNIT_DIR}" || return 1
  runtime_unit_identity_is_owned || return 1
  runtime_race_active=false
  runtime_race_foreign_identity=""
  runtime_race_foreign_hash=""
}

replace_runtime_unit_for_precommit_probe() {
  runtime_unit_identity_is_owned || return 1
  runtime_race_backup="$(
    mktemp "${RUNTIME_UNIT_DIR}/.${UNIT}.race-backup.XXXXXXXX"
  )" || return 1
  rm -f -- "${runtime_race_backup}" || return 1
  ln -- "${RUNTIME_UNIT_PATH}" "${runtime_race_backup}" || return 1
  [[ $(stat -c '%d:%i' -- "${runtime_race_backup}") == \
    "${runtime_unit_identity}" ]] || return 1
  runtime_race_active=true

  runtime_race_foreign_stage="$(
    mktemp "${RUNTIME_UNIT_DIR}/.${UNIT}.race-foreign.XXXXXXXX"
  )" || return 1
  runtime_race_foreign_identity="$(
    stat -c '%d:%i' -- "${runtime_race_foreign_stage}"
  )" || return 1
  cat > "${runtime_race_foreign_stage}" <<EOF
[Unit]
Description=encoder-recorder-installer-integration-foreign-runtime-unit

[Service]
Type=simple
User=nobody
ExecStart=/usr/bin/false

[Install]
# Keep enablement semantics equivalent while the foreign inode is present.
WantedBy=multi-user.target
EOF
  chmod 0644 "${runtime_race_foreign_stage}" || return 1
  runtime_race_foreign_hash="$(
    sha256sum "${runtime_race_foreign_stage}" | awk 'NR == 1 { print $1 }'
  )" || return 1
  sync -f "${runtime_race_foreign_stage}" || return 1
  mv -Tf -- "${runtime_race_foreign_stage}" "${RUNTIME_UNIT_PATH}" || return 1
  runtime_race_foreign_stage=""
  sync -f "${RUNTIME_UNIT_DIR}" || return 1
  [[ $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == \
    "${runtime_race_foreign_identity}" ]]
}

replace_runtime_unit_for_cleanup_probe() {
  local report_parent=""

  [[ -n ${cleanup_runtime_race_report} &&
    ${cleanup_runtime_race_report} == \
      /var/tmp/autostream-encoder-recorder-installer-test.*/* &&
    ! -e ${cleanup_runtime_race_report} &&
    ! -L ${cleanup_runtime_race_report} ]] || return 1
  report_parent="$(dirname -- "${cleanup_runtime_race_report}")" || return 1
  [[ -d ${report_parent} &&
    ! -L ${report_parent} &&
    $(stat -c '%U:%G:%a' -- "${report_parent}") == "root:root:700" ]] || \
    return 1
  replace_runtime_unit_for_precommit_probe || return 1
  if ! install -o root -g root -m 0600 /dev/null \
    "${cleanup_runtime_race_report}" ||
    ! printf '%s\t%s\t%s\n' \
      "${runtime_race_backup}" \
      "${runtime_race_foreign_identity}" \
      "${runtime_race_foreign_hash}" > "${cleanup_runtime_race_report}"; then
    restore_runtime_sync_race
    return 1
  fi
}

cleanup() {
  local exit_code=$?
  local cleanup_expected_unit_absent=false
  local cleanup_failed=false
  local cleanup_fragment_path=""
  local cleanup_load_state=""
  local current_pid_start_time=""
  local runtime_unit_identity_matches=false
  set +e
  if [[ ${runtime_unit_path_owned} == true ||
    ${service_start_attempted} == true ]]; then
    cleanup_expected_unit_absent=true
  fi
  if [[ ${runtime_race_active} == true ]] &&
    ! restore_runtime_sync_race; then
    cleanup_failed=true
  fi
  if [[ ${runtime_unit_path_owned} == true &&
    -n ${runtime_unit_identity} &&
    -f ${RUNTIME_UNIT_PATH} &&
    ! -L ${RUNTIME_UNIT_PATH} &&
    $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == "${runtime_unit_identity}" ]]; then
    runtime_unit_identity_matches=true
  fi
  if [[ ${runtime_unit_path_owned} == true &&
    ${runtime_unit_identity_matches} == false ]]; then
    cleanup_failed=true
    printf '%s\n' \
      "encoder-recorder installer integration test: cleanup could not prove runtime unit ownership" >&2
  fi
  if [[ ${service_start_attempted} == true &&
    ${runtime_unit_identity_matches} == true ]]; then
    if systemctl stop "${UNIT}" >/dev/null 2>&1; then
      service_started_by_fixture=false
      old_pid=""
      old_pid_start_time=""
    else
      cleanup_failed=true
      printf '%s\n' \
        "encoder-recorder installer integration test: cleanup failed to stop ${UNIT}" >&2
    fi
  fi
  if [[ ${service_started_by_fixture} == true &&
    -n ${old_pid} && -n ${old_pid_start_time} ]]; then
    current_pid_start_time="$(read_proc_pid_start_time "${old_pid}" 2>/dev/null || true)"
    if [[ -n ${current_pid_start_time} &&
      ${current_pid_start_time} == "${old_pid_start_time}" ]]; then
      if kill "${old_pid}" >/dev/null 2>&1; then
        service_started_by_fixture=false
        old_pid=""
        old_pid_start_time=""
      else
        cleanup_failed=true
      fi
    elif [[ -n ${current_pid_start_time} ]]; then
      cleanup_failed=true
      printf '%s\n' \
        "encoder-recorder installer integration test: cleanup fallback refused a reused PID ${old_pid}" >&2
    fi
  fi
  if [[ -n ${cleanup_runtime_pre_remove_hook} ]]; then
    if ! "${cleanup_runtime_pre_remove_hook}"; then
      cleanup_failed=true
      printf '%s\n' \
        "encoder-recorder installer integration test: cleanup runtime race hook failed" >&2
    fi
    cleanup_runtime_pre_remove_hook=""
  fi
  if [[ ${unit_enable_cleanup_needed} == true &&
    (${runtime_unit_path_owned} == false ||
      ${runtime_unit_identity_matches} == true) ]]; then
    if ! systemctl disable "${UNIT}" >/dev/null 2>&1; then
      cleanup_failed=true
    fi
  fi
  if [[ ${runtime_unit_temp_owned} == true && -n ${RUNTIME_UNIT_TEMP} ]]; then
    if ! rm -f -- "${RUNTIME_UNIT_TEMP}"; then
      cleanup_failed=true
    fi
  fi
  if [[ ${runtime_unit_path_owned} == true &&
    ${runtime_unit_identity_matches} == true ]]; then
    if ! runtime_unit_identity_is_owned; then
      cleanup_failed=true
      runtime_unit_identity_matches=false
      printf '%s\n' \
        "encoder-recorder installer integration test: cleanup could not prove runtime unit ownership before removal" >&2
    else
      if ! rm -f -- "${RUNTIME_UNIT_PATH}"; then
        cleanup_failed=true
        printf '%s\n' \
          "encoder-recorder installer integration test: cleanup failed to remove ${RUNTIME_UNIT_PATH}" >&2
      fi
      if ! systemctl daemon-reload >/dev/null 2>&1; then
        cleanup_failed=true
        printf '%s\n' \
          "encoder-recorder installer integration test: cleanup daemon-reload failed" >&2
      fi
    fi
  fi
  if [[ ${unit_path_owned} == true ]]; then
    rm -f -- "${UNIT_PATH}"
  fi
  if [[ ${public_binary_owned} == true ]]; then
    rm -f -- "${PUBLIC_BINARY}"
  fi
  if [[ ${public_alias_owned} == true ]]; then
    rm -f -- "${PUBLIC_ALIAS}"
  fi
  if [[ ${env_path_owned} == true ]]; then
    rm -f -- "${ENV_PATH}"
  fi
  if [[ ${shared_host_setup_lock_owned} == true ]]; then
    rm -f -- "${SHARED_HOST_SETUP_LOCK}"
  fi
  if [[ ${target_lock_owned} == true ]]; then
    rm -f -- "${TARGET_LOCK}"
  fi
  if [[ ${config_dir_owned} == true ]]; then
    rm -rf -- "${CONFIG_DIR}"
  fi
  if [[ ${state_dir_owned} == true ]]; then
    rm -rf -- "${STATE_DIR}"
  fi
  if [[ ${archive_dir_owned} == true ]]; then
    rm -rf -- "${ARCHIVE_DIR}"
  fi
  if [[ ${managed_root_owned} == true ]]; then
    rm -rf -- "${MANAGED_ROOT}"
  fi
  if [[ ${install_backup_root_owned} == true ]]; then
    rm -rf -- "${INSTALL_BACKUP_ROOT}"
  fi
  if [[ ${root_unpack_owned} == true ]]; then
    rm -rf -- /unpack
  fi
  if [[ ${recovery_path_owned} == true && -n ${RECOVERY_PATH} ]]; then
    rm -rf -- "${RECOVERY_PATH}"
  fi
  if [[ ${autostream_account_owned} == true ]]; then
    userdel autostream >/dev/null 2>&1
    groupdel autostream >/dev/null 2>&1
  fi
  if [[ ${work_dir_owned} == true ]]; then
    rm -rf -- "${WORK_DIR}"
  fi
  if [[ ${cleanup_expected_unit_absent} == true ]]; then
    if systemctl is-active --quiet "${UNIT}" >/dev/null 2>&1; then
      cleanup_failed=true
      printf '%s\n' \
        "encoder-recorder installer integration test: cleanup left ${UNIT} active" >&2
    fi
    cleanup_load_state="$(systemctl show --property LoadState --value "${UNIT}" 2>/dev/null || true)"
    cleanup_fragment_path="$(systemctl show --property FragmentPath --value "${UNIT}" 2>/dev/null || true)"
    if [[ ${cleanup_load_state} != "not-found" || -n ${cleanup_fragment_path} ]]; then
      cleanup_failed=true
      printf '%s\n' \
        "encoder-recorder installer integration test: cleanup left ${UNIT} loaded" >&2
    fi
  fi
  if [[ ${cleanup_failed} == true && ${exit_code} -eq 0 ]]; then
    exit_code=1
  fi
  exit "${exit_code}"
}
trap cleanup EXIT

if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_PROBE:-} == "1" ]]; then
  [[ -n ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_ID:-} &&
    -f ${RUNTIME_UNIT_PATH} &&
    ! -L ${RUNTIME_UNIT_PATH} &&
    $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == \
      "${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_ID}" ]] || \
    die "cleanup race probe could not adopt the expected runtime unit"
  runtime_unit_path_owned=true
  runtime_unit_identity="${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_ID}"
  cleanup_runtime_race_report="${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_REPORT:-}"
  cleanup_runtime_pre_remove_hook=replace_runtime_unit_for_cleanup_probe
  exit 0
fi

for path in \
  "${UNIT_PATH}" \
  "${RUNTIME_UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${CONFIG_DIR}" \
  "${STATE_DIR}" \
  "${ARCHIVE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  /opt/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /etc/autostream \
  "${SHARED_HOST_SETUP_LOCK}" \
  "${TARGET_LOCK}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || die "runner is not clean at ${path}"
done
preflight_load_state="$(systemctl show --property LoadState --value "${UNIT}" 2>/dev/null || true)"
preflight_fragment_path="$(systemctl show --property FragmentPath --value "${UNIT}" 2>/dev/null || true)"
[[ ${preflight_load_state} == "not-found" && -z ${preflight_fragment_path} ]] || \
  die "runner already has a loaded ${UNIT}"
systemctl is-active --quiet "${UNIT}" &&
  die "runner already has an active ${UNIT}"
preflight_enabled_state="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"
[[ -z ${preflight_enabled_state} ||
  ${preflight_enabled_state} == "disabled" ||
  ${preflight_enabled_state} == "not-found" ]] || \
  die "runner already has an enabled ${UNIT}"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "runner already has an autostream account"
fi
[[ ! -e /unpack && ! -L /unpack ]] || die "runner is not clean at /unpack"
if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_PREFLIGHT_PROBE:-} == "1" ]]; then
  die "preflight ownership probe unexpectedly passed"
fi
preflight_complete=true

assert_runtime_unit_file() {
  [[ -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]] || \
    die "runtime unit path is missing or unsafe"
  [[ $(stat -c '%U:%G:%a' -- "${RUNTIME_UNIT_PATH}") == "root:root:644" ]] || \
    die "runtime systemd unit ownership or mode is invalid"
}

assert_owned_runtime_unit_identity() {
  runtime_unit_identity_is_owned || \
    die "runtime unit identity changed or is not strictly fixture-owned"
  assert_runtime_unit_file
}

create_legacy_runtime_unit() {
  [[ ${runtime_unit_path_owned} == false ]] || \
    die "legacy runtime unit path is already fixture-owned"
  RUNTIME_UNIT_TEMP="$(mktemp "${RUNTIME_UNIT_DIR}/.${UNIT}.fixture.XXXXXXXX")"
  runtime_unit_temp_owned=true
  install -o root -g root -m 0644 "${UNIT_PATH}" "${RUNTIME_UNIT_TEMP}"
  sync -f "${RUNTIME_UNIT_TEMP}"
  if ! ln -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"; then
    die "could not atomically claim the legacy runtime unit path"
  fi
  runtime_unit_path_owned=true
  runtime_unit_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  rm -f -- "${RUNTIME_UNIT_TEMP}"
  runtime_unit_temp_owned=false
  RUNTIME_UNIT_TEMP=""
  sync -f "${RUNTIME_UNIT_DIR}"
  assert_owned_runtime_unit_identity
  cmp -s -- "${UNIT_PATH}" "${RUNTIME_UNIT_PATH}" || \
    die "atomic runtime unit creation changed the private unit"
}

sync_private_unit_to_runtime() {
  [[ ${unit_path_owned} == true && ${runtime_unit_path_owned} == true ]] || \
    die "cannot synchronize a runtime unit not owned by the fixture"
  assert_owned_runtime_unit_identity
  [[ -f ${UNIT_PATH} && ! -L ${UNIT_PATH} ]] || \
    die "private systemd unit is missing or unsafe"
  [[ -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]] || \
    die "owned runtime systemd unit is missing or unsafe"
  RUNTIME_UNIT_TEMP="$(mktemp "${RUNTIME_UNIT_DIR}/.${UNIT}.fixture.XXXXXXXX")"
  runtime_unit_temp_owned=true
  install -o root -g root -m 0644 "${UNIT_PATH}" "${RUNTIME_UNIT_TEMP}"
  cmp -s -- "${UNIT_PATH}" "${RUNTIME_UNIT_TEMP}" || \
    die "runtime unit staging changed the private unit"
  sync -f "${RUNTIME_UNIT_TEMP}"
  if [[ -n ${runtime_sync_precommit_hook} ]] &&
    ! "${runtime_sync_precommit_hook}"; then
    rm -f -- "${RUNTIME_UNIT_TEMP}"
    runtime_unit_temp_owned=false
    RUNTIME_UNIT_TEMP=""
    return 76
  fi
  if ! runtime_unit_identity_is_owned; then
    rm -f -- "${RUNTIME_UNIT_TEMP}"
    runtime_unit_temp_owned=false
    RUNTIME_UNIT_TEMP=""
    return 75
  fi
  mv -Tf -- "${RUNTIME_UNIT_TEMP}" "${RUNTIME_UNIT_PATH}"
  runtime_unit_temp_owned=false
  RUNTIME_UNIT_TEMP=""
  runtime_unit_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  sync -f "${RUNTIME_UNIT_DIR}"
  assert_owned_runtime_unit_identity
  cmp -s -- "${UNIT_PATH}" "${RUNTIME_UNIT_PATH}" || \
    die "managed runtime unit does not match the private unit"
  systemctl daemon-reload
}

assert_legacy_pid1_state() {
  local scenario=$1
  local runtime_unit_now fragment_now exec_start_now user_now pid_now
  local pid_start_time_now=""

  assert_owned_runtime_unit_identity
  runtime_unit_now="$(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
  fragment_now="$(systemctl show --property FragmentPath --value "${UNIT}")"
  exec_start_now="$(systemctl show --property ExecStart --value "${UNIT}")"
  user_now="$(systemctl show --property User --value "${UNIT}")"
  pid_now="$(systemctl show --property MainPID --value "${UNIT}")"

  [[ ${runtime_unit_now} == "${legacy_runtime_unit_before}" ]] || \
    die "${scenario} changed the legacy runtime unit shadow"
  [[ ${fragment_now} == "${legacy_fragment_before}" ]] || \
    die "${scenario} changed PID1 FragmentPath"
  [[ ${exec_start_now} == *"path=/usr/bin/sleep"* &&
    ${exec_start_now} == *"argv[]=/usr/bin/sleep infinity"* ]] || \
    die "${scenario} changed PID1 ExecStart command"
  [[ ${user_now} == "${legacy_user_before}" ]] || \
    die "${scenario} changed PID1 User"
  [[ ${pid_now} == "${old_pid}" ]] || \
    die "${scenario} replaced the running legacy process"
  pid_start_time_now="$(read_proc_pid_start_time "${old_pid}")" || \
    die "${scenario} could not read the legacy process identity"
  [[ ${pid_start_time_now} == "${old_pid_start_time}" ]] || \
    die "${scenario} observed PID reuse for the legacy process"
  kill -0 "${old_pid}" || die "${scenario} stopped the legacy process"
}

assert_migrated_pid1_state() {
  local scenario=$1
  local fragment_now exec_start_now user_now pid_now

  assert_owned_runtime_unit_identity
  fragment_now="$(systemctl show --property FragmentPath --value "${UNIT}")"
  exec_start_now="$(systemctl show --property ExecStart --value "${UNIT}")"
  user_now="$(systemctl show --property User --value "${UNIT}")"
  pid_now="$(systemctl show --property MainPID --value "${UNIT}")"

  [[ ${fragment_now} == "${RUNTIME_UNIT_PATH}" ]] || \
    die "${scenario} PID1 FragmentPath does not use the owned runtime unit"
  [[ ${exec_start_now} == *"path=${PUBLIC_BINARY}"* &&
    ${exec_start_now} == *"argv[]=${PUBLIC_BINARY}"* ]] || \
    die "${scenario} PID1 ExecStart does not use the stable public binary"
  [[ ${user_now} == "autostream" ]] || \
    die "${scenario} PID1 User is not autostream"
  [[ ${pid_now} == "${old_pid}" ]] || \
    die "${scenario} replaced the running legacy process"
  [[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
    "$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')" ]] || \
    die "${scenario} runtime unit does not match the private unit"
  kill -0 "${old_pid}" || die "${scenario} stopped the legacy process"
}

unit_path_owned=true
cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=encoder-recorder-installer-integration-runtime-sentinel

[Service]
Type=simple
User=root
ExecStart=/usr/bin/sleep infinity
Restart=no

[Install]
# Keep cleanup race enablement semantics equivalent to the foreign probe unit.
WantedBy=multi-user.target
EOF
chmod 0644 "${UNIT_PATH}"
create_legacy_runtime_unit
systemctl daemon-reload
service_start_attempted=true
systemctl start "${UNIT}"
service_started_by_fixture=true
old_pid="$(systemctl show --property MainPID --value "${UNIT}")"
[[ ${old_pid} =~ ^[1-9][0-9]*$ ]] || die "runtime sentinel service did not start"
old_pid_start_time="$(read_proc_pid_start_time "${old_pid}")"
[[ ${old_pid_start_time} =~ ^[0-9]+$ ]] || \
  die "runtime sentinel PID start time is unavailable"
runtime_sentinel_identity_before="${runtime_unit_identity}"
runtime_sentinel_hash_before="$(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
runtime_sentinel_fragment_before="$(systemctl show --property FragmentPath --value "${UNIT}")"
runtime_sentinel_exec_start_before="$(systemctl show --property ExecStart --value "${UNIT}")"
runtime_sentinel_user_before="$(systemctl show --property User --value "${UNIT}")"
runtime_sentinel_enabled_before="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"
rm -f -- "${UNIT_PATH}"
unit_path_owned=false

cleanup_race_report="${WORK_DIR}/cleanup-runtime-race.report"
set +e
AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS=1 \
  AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_PROBE=1 \
  AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_ID="${runtime_sentinel_identity_before}" \
  AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_CLEANUP_RACE_REPORT="${cleanup_race_report}" \
  bash "${SCRIPT_DIR}/test-install-autostream-encoder-recorder-integration.sh" \
  > "${WORK_DIR}/cleanup-runtime-race.out" 2>&1
cleanup_race_status=$?
set -e
[[ ${cleanup_race_status} -eq 1 ]] || \
  die "cleanup runtime race did not promote a successful exit to failure"
[[ -f ${cleanup_race_report} && ! -L ${cleanup_race_report} &&
  $(stat -c '%U:%G:%a' -- "${cleanup_race_report}") == "root:root:600" ]] || \
  die "cleanup runtime race report is missing or unsafe"
IFS=$'\t' read -r \
  cleanup_race_backup \
  cleanup_race_foreign_identity \
  cleanup_race_foreign_hash < "${cleanup_race_report}"
[[ ${cleanup_race_backup} == \
    "${RUNTIME_UNIT_DIR}/.${UNIT}.race-backup."* &&
  -f ${cleanup_race_backup} &&
  ! -L ${cleanup_race_backup} &&
  $(stat -c '%d:%i' -- "${cleanup_race_backup}") == \
    "${runtime_sentinel_identity_before}" ]] || \
  die "cleanup runtime race did not preserve the owned inode for recovery"
[[ $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == \
  "${cleanup_race_foreign_identity}" ]] || \
  die "cleanup runtime race removed or replaced the foreign inode"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "${cleanup_race_foreign_hash}" ]] || \
  die "cleanup runtime race changed the foreign runtime unit hash"
[[ $(systemctl show --property FragmentPath --value "${UNIT}") == \
  "${runtime_sentinel_fragment_before}" ]] || \
  die "cleanup runtime race changed PID1 FragmentPath"
[[ $(systemctl show --property ExecStart --value "${UNIT}") == \
  "${runtime_sentinel_exec_start_before}" ]] || \
  die "cleanup runtime race changed PID1 ExecStart"
[[ $(systemctl show --property User --value "${UNIT}") == \
  "${runtime_sentinel_user_before}" ]] || \
  die "cleanup runtime race changed PID1 User"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "cleanup runtime race changed the runtime sentinel PID"
[[ $(systemctl is-enabled "${UNIT}" 2>/dev/null || true) == \
  "${runtime_sentinel_enabled_before}" ]] || \
  die "cleanup runtime race changed the runtime sentinel enabled state"
kill -0 "${old_pid}" || die "cleanup runtime race stopped the runtime sentinel"
mv -Tf -- "${cleanup_race_backup}" "${RUNTIME_UNIT_PATH}"
sync -f "${RUNTIME_UNIT_DIR}"
[[ $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == \
  "${runtime_sentinel_identity_before}" ]] || \
  die "cleanup runtime race recovery did not restore the owned inode"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "${runtime_sentinel_hash_before}" ]] || \
  die "cleanup runtime race recovery changed the runtime sentinel hash"
rm -f -- "${cleanup_race_report}"

set +e
AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS=1 \
  AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_PREFLIGHT_PROBE=1 bash \
  "${SCRIPT_DIR}/test-install-autostream-encoder-recorder-integration.sh" \
  > "${WORK_DIR}/preflight-conflict.out" 2>&1
preflight_probe_status=$?
set -e
[[ ${preflight_probe_status} -ne 0 ]] || \
  die "preflight conflict probe unexpectedly succeeded"
grep -F -- "runner is not clean at ${RUNTIME_UNIT_PATH}" \
  "${WORK_DIR}/preflight-conflict.out" >/dev/null || \
  die "preflight conflict probe did not reject the runtime sentinel"
[[ ! -e ${UNIT_PATH} && ! -L ${UNIT_PATH} ]] || \
  die "preflight conflict recreated the private unit"
[[ $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == \
  "${runtime_sentinel_identity_before}" ]] || \
  die "preflight conflict changed the runtime sentinel inode"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "${runtime_sentinel_hash_before}" ]] || \
  die "preflight conflict changed the runtime sentinel hash"
[[ $(systemctl show --property FragmentPath --value "${UNIT}") == \
  "${runtime_sentinel_fragment_before}" ]] || \
  die "preflight conflict changed the runtime sentinel FragmentPath"
[[ $(systemctl show --property ExecStart --value "${UNIT}") == \
  "${runtime_sentinel_exec_start_before}" ]] || \
  die "preflight conflict changed the runtime sentinel ExecStart"
[[ $(systemctl show --property User --value "${UNIT}") == \
  "${runtime_sentinel_user_before}" ]] || \
  die "preflight conflict changed the runtime sentinel User"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "preflight conflict changed the runtime sentinel PID"
[[ $(systemctl is-enabled "${UNIT}" 2>/dev/null || true) == \
  "${runtime_sentinel_enabled_before}" ]] || \
  die "preflight conflict changed the runtime sentinel enabled state"
kill -0 "${old_pid}" || die "preflight conflict stopped the runtime sentinel"

systemctl stop "${UNIT}"
service_start_attempted=false
service_started_by_fixture=false
assert_owned_runtime_unit_identity
rm -f -- "${RUNTIME_UNIT_PATH}"
runtime_unit_path_owned=false
runtime_unit_identity=""
old_pid=""
old_pid_start_time=""
systemctl daemon-reload

install -d -o root -g root -m 0755 \
  "${ARTIFACTS_DIR}" \
  "${EXTRACTED_ROOT}/bin" \
  "${EXTRACTED_ROOT}/systemd"
install -o root -g root -m 0755 "${INSTALLER_SOURCE}" \
  "${EXTRACTED_ROOT}/install-autostream-encoder-recorder"

cat > "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' 'autostream-encoder-recorder v9.9.9'
  printf '%s\n' 'commit: 0000000000000000000000000000000000000000'
  printf '%s\n' 'build_date: 2026-01-01T00:00:00Z'
  exit 0
fi
exec /usr/bin/sleep infinity
EOF
chmod 0755 "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder"
cp "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder" \
  "${EXTRACTED_ROOT}/bin/encoder-recorder"
chmod 0755 "${EXTRACTED_ROOT}/bin/encoder-recorder"

cat > "${EXTRACTED_ROOT}/systemd/autostream-encoder-recorder.service.example" <<'EOF'
[Unit]
Description=AutoStream Encoder Recorder integration fixture

[Service]
Type=simple
User=autostream
Group=autostream
EnvironmentFile=-/etc/autostream/encoder-recorder.env
ExecStart=/usr/local/bin/autostream-encoder-recorder
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
printf '%s\n' 'AUTOSTREAM_BIND_ADDR=127.0.0.1:18081' \
  > "${EXTRACTED_ROOT}/.env.example"

jq -n \
  --arg version "${VERSION}" \
  --arg commit "${FIXTURE_COMMIT}" \
  --arg build_date "${FIXTURE_BUILD_DATE}" \
  --arg arch "amd64" \
  --arg archive_name "${ARTIFACT_ID}.tar.gz" \
  --arg archive_root "${ARTIFACT_ID}" \
  '{
    schema_version: 1,
    component: "encoder-recorder",
    source_version: $version,
    commit: $commit,
    build_date: $build_date,
    platform: {
      os: "linux",
      arch: $arch
    },
    archive: {
      name: $archive_name,
      root: $archive_root
    },
    compatibility: {
      minimum_agent_version: "v1.0.0",
      minimum_panel_version: null,
      rollback_compatible: true,
      database_schema: "none"
    }
  }' > "${EXTRACTED_ROOT}/artifact-manifest.json"

(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
archive_sha256="$(sha256sum "${ARCHIVE}" | awk 'NR == 1 { print $1 }')"
[[ ! -e ${ARCHIVE}.sha256 && ! -L ${ARCHIVE}.sha256 ]] || \
  die "archive-only fixture unexpectedly contains an archive sidecar"
[[ ! -e ${ARTIFACTS_DIR}/release-manifest.json &&
  ! -L ${ARTIFACTS_DIR}/release-manifest.json ]] || \
  die "archive-only fixture unexpectedly contains release-manifest.json"
[[ ! -e ${ARTIFACTS_DIR}/release-manifest.json.sha256 &&
  ! -L ${ARTIFACTS_DIR}/release-manifest.json.sha256 ]] || \
  die "archive-only fixture unexpectedly contains a release manifest sidecar"

readonly VALID_ARCHIVE="${WORK_DIR}/${ARTIFACT_ID}.valid.tar.gz"
install -o root -g root -m 0600 "${ARCHIVE}" "${VALID_ARCHIVE}"

rebuild_fixture_archive() {
  tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
}

restore_valid_fixture() {
  rm -rf -- "${EXTRACTED_ROOT}"
  install -o root -g root -m 0600 "${VALID_ARCHIVE}" "${ARCHIVE}"
  tar -C "${ARTIFACTS_DIR}" -xzf "${ARCHIVE}"
  [[ $(sha256sum "${ARCHIVE}" | awk 'NR == 1 { print $1 }') == "${archive_sha256}" ]] || \
    die "could not restore the valid archive-only fixture"
}

assert_no_persistent_installer_mutation() {
  local scenario=$1
  local path
  if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
    die "${scenario} mutated the service account"
  fi
  [[ ! -e ${MANAGED_ROOT} && ! -L ${MANAGED_ROOT} ]] || \
    die "${scenario} created the managed root"
  [[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || \
    die "${scenario} created the state directory"
  [[ ! -e ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} ]] || \
    die "${scenario} created the archive directory"
  [[ ! -e ${INSTALL_BACKUP_ROOT} && ! -L ${INSTALL_BACKUP_ROOT} ]] || \
    die "${scenario} created the installer backup directory"
  [[ ! -e ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} ]] || \
    die "${scenario} created the canonical public binary"
  [[ ! -e ${PUBLIC_ALIAS} && ! -L ${PUBLIC_ALIAS} ]] || \
    die "${scenario} created the public alias"
  [[ ! -e ${ENV_PATH} && ! -L ${ENV_PATH} ]] || \
    die "${scenario} created the environment file"
  [[ ! -e ${UNIT_PATH} && ! -L ${UNIT_PATH} ]] || \
    die "${scenario} created the systemd unit"
  for path in /opt/autostream /var/lib/autostream /var/backups/autostream /etc/autostream; do
    [[ ! -e ${path} && ! -L ${path} ]] || \
      die "${scenario} left transactional parent directory ${path}"
  done
}

tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" \
  "${ARTIFACT_ID}" \
  "${ARTIFACT_ID}/.env.example"
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/duplicate-archive-path.out" 2>&1
duplicate_archive_path_status=$?
set -e
[[ ${duplicate_archive_path_status} -ne 0 ]] || \
  die "installer accepted an archive with a duplicate path"
grep -F -- "release archive contains duplicate paths" \
  "${WORK_DIR}/duplicate-archive-path.out" >/dev/null || \
  die "duplicate archive path did not fail at the archive boundary"
assert_no_persistent_installer_mutation "duplicate archive path"
restore_valid_fixture

python3 - "${ARCHIVE}" "${ARTIFACT_ID}" <<'PY'
import io
import sys
import tarfile

archive_path, artifact_id = sys.argv[1:]
with tarfile.open(archive_path, "w:gz") as archive:
    regular = tarfile.TarInfo(f"{artifact_id}/canonical-alias")
    regular.mode = 0o644
    regular.size = 0
    archive.addfile(regular, io.BytesIO(b""))

    directory = tarfile.TarInfo(f"{artifact_id}/canonical-alias/")
    directory.type = tarfile.DIRTYPE
    directory.mode = 0o755
    archive.addfile(directory)
PY
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/trailing-slash-archive-alias.out" 2>&1
trailing_slash_archive_alias_status=$?
set -e
[[ ${trailing_slash_archive_alias_status} -ne 0 ]] || \
  die "installer accepted a trailing-slash archive path alias"
grep -F -- "release archive contains duplicate paths" \
  "${WORK_DIR}/trailing-slash-archive-alias.out" >/dev/null || \
  die "trailing-slash archive alias did not fail at the canonical duplicate boundary"
assert_no_persistent_installer_mutation "trailing-slash archive alias"
restore_valid_fixture

printf '%s\n' 'corrupt-after-checksum' >> "${EXTRACTED_ROOT}/.env.example"
rebuild_fixture_archive
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/corrupt-inner-file.out" 2>&1
corrupt_inner_file_status=$?
set -e
[[ ${corrupt_inner_file_status} -ne 0 ]] || \
  die "installer accepted an archive with a corrupt checksummed file"
grep -F -- ".env.example: FAILED" "${WORK_DIR}/corrupt-inner-file.out" >/dev/null || \
  die "corrupt archive did not fail at the inner checksum boundary"
assert_no_persistent_installer_mutation "corrupt inner file"
restore_valid_fixture

invalid_manifest_stage="$(mktemp "${EXTRACTED_ROOT}/.artifact-manifest.invalid.XXXXXXXX")"
jq '.source_version = "v9.9.8" | .platform.arch = "arm64"' \
  "${EXTRACTED_ROOT}/artifact-manifest.json" > "${invalid_manifest_stage}"
mv -Tf -- "${invalid_manifest_stage}" "${EXTRACTED_ROOT}/artifact-manifest.json"
(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
rebuild_fixture_archive
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/invalid-artifact-manifest.out" 2>&1
invalid_artifact_manifest_status=$?
set -e
[[ ${invalid_artifact_manifest_status} -ne 0 ]] || \
  die "installer accepted mismatched artifact metadata"
grep -F -- "artifact-manifest.json does not describe this exact Encoder Recorder artifact" \
  "${WORK_DIR}/invalid-artifact-manifest.out" >/dev/null || \
  die "mismatched artifact metadata did not fail at the manifest boundary"
assert_no_persistent_installer_mutation "mismatched artifact metadata"
restore_valid_fixture

sed -i \
  's/commit: 0000000000000000000000000000000000000000/commit: 1111111111111111111111111111111111111111/' \
  "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder"
cp "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder" \
  "${EXTRACTED_ROOT}/bin/encoder-recorder"
chmod 0755 \
  "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder" \
  "${EXTRACTED_ROOT}/bin/encoder-recorder"
(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
rebuild_fixture_archive
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/binary-identity-mismatch.out" 2>&1
binary_identity_mismatch_status=$?
set -e
[[ ${binary_identity_mismatch_status} -ne 0 ]] || \
  die "installer accepted a binary identity mismatch"
grep -F -- "Encoder Recorder binary identity does not exactly match artifact-manifest.json" \
  "${WORK_DIR}/binary-identity-mismatch.out" >/dev/null || \
  die "binary identity mismatch did not fail at the binary verification boundary"
assert_no_persistent_installer_mutation "binary identity mismatch"
restore_valid_fixture

printf '%s\n' \
  '#!/bin/sh' \
  "printf '%s\n' reached > '${WORK_DIR}/mktemp-shim.reached'" \
  'exit 73' > "${WORK_DIR}/failing-mktemp"
chmod 0755 "${WORK_DIR}/failing-mktemp"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${WORK_DIR}/failing-mktemp' /usr/bin/mktemp && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/mktemp-failure.out" 2>&1
mktemp_failure_status=$?
set -e
[[ ${mktemp_failure_status} -eq 73 ]] || die "installer did not preserve the INPUT_STAGE mktemp failure status"
[[ $(< "${WORK_DIR}/mktemp-shim.reached") == "reached" ]] || \
  die "mktemp failure injection did not reach the mounted shim"
if [[ -e /unpack || -L /unpack ]]; then
  root_unpack_owned=true
  die "mktemp failure created a root-level /unpack path"
fi
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "mktemp failure mutated the service account"
fi

public_binary_owned=true
ln -s -- /usr/bin/false "${PUBLIC_BINARY}"
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/late-public-preflight-failure.out" 2>&1
late_public_preflight_status=$?
set -e
[[ ${late_public_preflight_status} -ne 0 ]] || \
  die "unexpected public path passed the late preflight"
grep -F -- "existing public symlink has an unexpected target: ${PUBLIC_BINARY}" \
  "${WORK_DIR}/late-public-preflight-failure.out" >/dev/null || \
  die "unexpected public path did not fail at the public-path preflight"
[[ -L ${PUBLIC_BINARY} && $(readlink -- "${PUBLIC_BINARY}") == "/usr/bin/false" ]] || \
  die "late public-path preflight changed its conflicting path"
rm -f -- "${PUBLIC_BINARY}"
public_binary_owned=false
assert_no_persistent_installer_mutation "late public-path preflight"
for unexpected_persistent_directory in \
  /opt/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /etc/autostream; do
  [[ ! -e ${unexpected_persistent_directory} && ! -L ${unexpected_persistent_directory} ]] || \
    die "late public-path preflight created a persistent directory: ${unexpected_persistent_directory}"
done

install -o root -g root -m 0755 /usr/bin/systemctl "${WORK_DIR}/real-systemctl"
printf '%s\n' \
  '#!/bin/sh' \
  'if [ "${1:-}" = "daemon-reload" ] && [ ! -e "'"${WORK_DIR}"'/fresh-late-failure-reached" ]; then' \
  "  : > '${WORK_DIR}/fresh-late-failure-reached'" \
  '  exit 74' \
  'fi' \
  'if [ "${1:-}" = "daemon-reload" ] && [ ! -e "'"${WORK_DIR}"'/cleanup-second-term-delivered" ]; then' \
  "  : > '${WORK_DIR}/cleanup-second-term-delivered'" \
  '  kill -TERM "$PPID"' \
  '  kill -TERM "$PPID"' \
  'fi' \
  "exec '${WORK_DIR}/real-systemctl' \"\$@\"" \
  > "${WORK_DIR}/fail-first-daemon-reload"
chmod 0755 "${WORK_DIR}/fail-first-daemon-reload"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${WORK_DIR}/fail-first-daemon-reload' /usr/bin/systemctl && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/fresh-late-failure.out" 2>&1
fresh_late_failure_status=$?
set -e
[[ ${fresh_late_failure_status} -eq 74 ]] || \
  die "fresh late-failure rollback probe returned an unexpected status"
[[ -f ${WORK_DIR}/fresh-late-failure-reached ]] || \
  die "fresh late-failure rollback probe did not reach daemon-reload"
[[ -f ${WORK_DIR}/cleanup-second-term-delivered ]] || \
  die "fresh late-failure rollback did not exercise a second TERM during cleanup"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "fresh late-failure rollback left the invocation-created service account"
fi
for path in \
  /opt/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /etc/autostream \
  "${MANAGED_ROOT}" \
  "${STATE_DIR}" \
  "${ARCHIVE_DIR}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${UNIT_PATH}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || \
    die "fresh late-failure rollback left persistent mutation ${path}"
done
[[ -f ${SHARED_HOST_SETUP_LOCK} && ! -L ${SHARED_HOST_SETUP_LOCK} &&
  $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
  -f ${TARGET_LOCK} && ! -L ${TARGET_LOCK} &&
  $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" ]] || \
  die "fresh late-failure rollback did not retain the safe permanent updater locks"
[[ $(stat -c '%U:%G:%a' -- /run/autostream-updater) == "root:root:700" ]] || \
  die "permanent updater lock directory is not root-only after rollback"
adopt_installer_paths

printf '%s\n' 'encoder-recorder shared host-setup lock sentinel' \
  > "${SHARED_HOST_SETUP_LOCK}"
chmod 0600 "${SHARED_HOST_SETUP_LOCK}"
shared_contention_locks_before="$(
  printf 'shared|%s|' \
    "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf 'target|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)"
(
  exec 7<>"${SHARED_HOST_SETUP_LOCK}"
  flock -n 7 || die "fixture could not acquire the shared host-setup lock"
  set +e
  "${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
    > "${WORK_DIR}/shared-lock-contention.out" 2>&1
  printf '%s\n' "$?" > "${WORK_DIR}/shared-lock-contention.status"
)
shared_contention_status="$(< "${WORK_DIR}/shared-lock-contention.status")"
[[ ${shared_contention_status} -eq 1 ]] || \
  die "installer ignored shared host-setup lock contention"
grep -Fx -- \
  "install-autostream-encoder-recorder: another AutoStream installer is provisioning shared host state" \
  "${WORK_DIR}/shared-lock-contention.out" >/dev/null || \
  die "shared host-setup lock contention did not report the exact installer error"
[[ "$(
  printf 'shared|%s|' \
    "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf 'target|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${shared_contention_locks_before}" ]] || \
  die "shared host-setup contention replaced or truncated a permanent lock"
assert_no_persistent_installer_mutation "shared host-setup lock contention"
for unexpected_persistent_directory in \
  /opt/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /etc/autostream; do
  [[ ! -e ${unexpected_persistent_directory} && ! -L ${unexpected_persistent_directory} ]] || \
    die "shared host-setup lock contention mutated transactional host state"
done

install -o root -g root -m 0755 /usr/sbin/groupadd "${WORK_DIR}/real-groupadd"
install -o root -g root -m 0755 /usr/sbin/useradd "${WORK_DIR}/real-useradd"
printf '%s\n' \
  '#!/bin/sh' \
  "'${WORK_DIR}/real-groupadd' \"\$@\"" \
  'status=$?' \
  '[ "${status}" -ne 0 ] || kill -TERM "$PPID"' \
  "[ \"\${status}\" -ne 0 ] || : > '${WORK_DIR}/groupadd-term-delivered'" \
  'exit "${status}"' \
  > "${WORK_DIR}/groupadd-term-probe"
printf '%s\n' \
  '#!/bin/sh' \
  "'${WORK_DIR}/real-useradd' \"\$@\"" \
  'status=$?' \
  '[ "${status}" -ne 0 ] || kill -TERM "$PPID"' \
  "[ \"\${status}\" -ne 0 ] || : > '${WORK_DIR}/useradd-term-delivered'" \
  'exit "${status}"' \
  > "${WORK_DIR}/useradd-term-probe"
chmod 0755 "${WORK_DIR}/groupadd-term-probe" "${WORK_DIR}/useradd-term-probe"

set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${WORK_DIR}/groupadd-term-probe' /usr/sbin/groupadd && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/groupadd-term.out" 2>&1
groupadd_term_status=$?
set -e
[[ ${groupadd_term_status} -eq 143 ]] || \
  die "groupadd TERM transaction exited with ${groupadd_term_status}, expected 143"
[[ -f ${WORK_DIR}/groupadd-term-delivered ]] || \
  die "groupadd TERM transaction did not receive its termination request"
assert_no_persistent_installer_mutation "groupadd TERM transaction rollback"
[[ "$(
  printf 'shared|%s|' \
    "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf 'target|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${shared_contention_locks_before}" ]] || \
  die "groupadd TERM transaction replaced or truncated a permanent lock"

"${WORK_DIR}/real-groupadd" --system autostream
preexisting_group_record="$(getent group autostream)"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${WORK_DIR}/useradd-term-probe' /usr/sbin/useradd && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/useradd-term.out" 2>&1
useradd_term_status=$?
set -e
[[ ${useradd_term_status} -eq 143 ]] || \
  die "useradd TERM transaction exited with ${useradd_term_status}, expected 143"
[[ -f ${WORK_DIR}/useradd-term-delivered ]] || \
  die "useradd TERM transaction did not receive its termination request"
id autostream >/dev/null 2>&1 && \
  die "useradd TERM transaction left the invocation-created service user"
[[ $(getent group autostream) == "${preexisting_group_record}" ]] || \
  die "useradd TERM transaction changed the pre-existing service group"
for useradd_term_path in \
  /opt/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /etc/autostream \
  "${MANAGED_ROOT}" \
  "${STATE_DIR}" \
  "${ARCHIVE_DIR}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${UNIT_PATH}"; do
  [[ ! -e ${useradd_term_path} && ! -L ${useradd_term_path} ]] || \
    die "useradd TERM transaction rollback left persistent mutation ${useradd_term_path}"
done
[[ "$(
  printf 'shared|%s|' \
    "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf 'target|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${shared_contention_locks_before}" ]] || \
  die "useradd TERM transaction replaced or truncated a permanent lock"
groupdel autostream
assert_no_persistent_installer_mutation "useradd TERM transaction rollback"

set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/fresh.out" 2>&1
fresh_status=$?
set -e
adopt_installer_paths
[[ ${fresh_status} -eq 0 ]] || die "fresh installer invocation failed"
[[ -L ${MANAGED_ROOT}/current ]] || die "fresh install did not activate current"
[[ -L ${PUBLIC_BINARY} && -L ${PUBLIC_ALIAS} ]] || \
  die "fresh install did not create stable public links"
[[ -f ${ENV_PATH} && ! -L ${ENV_PATH} ]] || die "fresh install did not seed the environment"
[[ $(stat -c '%U:%G:%a' -- "${ENV_PATH}") == "root:root:640" ]] || \
  die "fresh environment ownership or mode is invalid"
[[ $(stat -c '%U:%G:%a' -- "${STATE_DIR}") == "autostream:autostream:750" ]] || \
  die "fresh state directory ownership or mode is invalid"
[[ $(stat -c '%U:%G:%a' -- "${ARCHIVE_DIR}") == "autostream:autostream:750" ]] || \
  die "fresh archive directory ownership or mode is invalid"
systemctl is-active --quiet "${UNIT}" && die "fresh installer unexpectedly started the service"
assert_not_enabled
grep -F -- "sudo systemctl enable --now ${UNIT}" "${WORK_DIR}/fresh.out" >/dev/null || \
  die "fresh install did not print the explicit first-start command"

fresh_env_sha256="$(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }')"
printf '%s\n' 'intentionally-invalid-archive-sidecar' > "${ARCHIVE}.sha256"
printf '%s\n' 'intentionally-invalid-external-manifest' \
  > "${ARTIFACTS_DIR}/release-manifest.json"
printf '%s\n' 'intentionally-invalid-manifest-sidecar' \
  > "${ARTIFACTS_DIR}/release-manifest.json.sha256"
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/ignored-external-metadata.out" 2>&1
ignored_external_metadata_status=$?
set -e
[[ ${ignored_external_metadata_status} -eq 0 ]] || \
  die "installer read unrelated external release metadata"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${fresh_env_sha256}" ]] || \
  die "external metadata ignore probe changed the fresh environment"
systemctl is-active --quiet "${UNIT}" && \
  die "external metadata ignore probe unexpectedly started the service"
assert_not_enabled
rm -f -- \
  "${ARCHIVE}.sha256" \
  "${ARTIFACTS_DIR}/release-manifest.json" \
  "${ARTIFACTS_DIR}/release-manifest.json.sha256"

[[ ${public_binary_owned} == true &&
  ${public_alias_owned} == true &&
  ${env_path_owned} == true &&
  ${unit_path_owned} == true &&
  ${state_dir_owned} == true &&
  ${archive_dir_owned} == true &&
  ${managed_root_owned} == true ]] || \
  die "fresh install path ownership was not captured"
rm -f -- "${PUBLIC_BINARY}"
public_binary_owned=false
rm -f -- "${PUBLIC_ALIAS}"
public_alias_owned=false
rm -f -- "${ENV_PATH}"
env_path_owned=false
rm -f -- "${UNIT_PATH}"
systemctl daemon-reload
unit_path_owned=false
rm -rf -- "${STATE_DIR}"
state_dir_owned=false
rm -rf -- "${ARCHIVE_DIR}"
archive_dir_owned=false
rm -rf -- "${MANAGED_ROOT}"
managed_root_owned=false
if [[ ${install_backup_root_owned} == true ]]; then
  rm -rf -- "${INSTALL_BACKUP_ROOT}"
  install_backup_root_owned=false
fi

state_dir_owned=true
archive_dir_owned=true
install -d -o autostream -g autostream -m 0750 "${STATE_DIR}" "${ARCHIVE_DIR}"
install -d -o root -g root -m 0750 /etc/autostream
env_path_owned=true
printf '%s\n' "${LEGACY_ENV_CONTENT}" > "${ENV_PATH}"
chmod 0640 "${ENV_PATH}"
config_dir_owned=true
install -d -o root -g root -m 0700 "${CONFIG_DIR}"
printf '%s\n' "${LEGACY_CONFIG_CONTENT}" > "${CONFIG_PATH}"
chmod 0600 "${CONFIG_PATH}"
public_binary_owned=true
printf '%s\n' "${LEGACY_BINARY_CONTENT}" > "${PUBLIC_BINARY}"
chmod 0755 "${PUBLIC_BINARY}"
public_alias_owned=true
printf '%s\n' "${LEGACY_ALIAS_CONTENT}" > "${PUBLIC_ALIAS}"
chmod 0755 "${PUBLIC_ALIAS}"
unit_path_owned=true
cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=${LEGACY_UNIT_CONTENT}

[Service]
Type=simple
User=root
ExecStart=/usr/bin/sleep infinity
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "${UNIT_PATH}"
create_legacy_runtime_unit
systemctl daemon-reload
service_start_attempted=true
systemctl start "${UNIT}"
service_started_by_fixture=true
old_pid="$(systemctl show --property MainPID --value "${UNIT}")"
[[ ${old_pid} =~ ^[1-9][0-9]*$ ]] || die "legacy service did not start"
old_pid_start_time="$(read_proc_pid_start_time "${old_pid}")"
[[ ${old_pid_start_time} =~ ^[0-9]+$ ]] || \
  die "legacy service PID start time is unavailable"
kill -0 "${old_pid}" || die "legacy service PID is not alive"
legacy_unit_file_state="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"
[[ ${legacy_unit_file_state} == "disabled" ]] || \
  die "legacy fixture must begin disabled, got ${legacy_unit_file_state:-unknown}"

readonly STATE_SENTINEL="${STATE_DIR}/installer-state-sentinel"
printf '%s\n' 'encoder-recorder-existing-state-preserve-exactly' > "${STATE_SENTINEL}"
chown autostream:autostream "${STATE_SENTINEL}"
chmod 0600 "${STATE_SENTINEL}"
chmod 0700 "${STATE_DIR}"
readonly ARCHIVE_SENTINEL="${ARCHIVE_DIR}/installer-archive-sentinel"
printf '%s\n' 'encoder-recorder-existing-archive-preserve-exactly' > "${ARCHIVE_SENTINEL}"
chown autostream:autostream "${ARCHIVE_SENTINEL}"
chmod 0600 "${ARCHIVE_SENTINEL}"
chmod 0700 "${ARCHIVE_DIR}"
state_directory_before="$(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}")"
state_sentinel_before="$(sha256sum "${STATE_SENTINEL}" | awk 'NR == 1 { print $1 }')"
state_listing_before="$(
  find "${STATE_DIR}" -mindepth 1 -maxdepth 1 -printf '%P:%y:%u:%g:%m\n' |
    LC_ALL=C sort |
    sha256sum |
    awk 'NR == 1 { print $1 }'
)"
archive_directory_before="$(stat -c '%d:%i:%u:%g:%a' -- "${ARCHIVE_DIR}")"
archive_sentinel_before="$(sha256sum "${ARCHIVE_SENTINEL}" | awk 'NR == 1 { print $1 }')"
archive_listing_before="$(
  find "${ARCHIVE_DIR}" -mindepth 1 -maxdepth 1 -printf '%P:%y:%u:%g:%m\n' |
    LC_ALL=C sort |
    sha256sum |
    awk 'NR == 1 { print $1 }'
)"
assert_existing_state_unchanged() {
  local scenario=$1
  [[ -d ${STATE_DIR} && ! -L ${STATE_DIR} &&
    $(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}") == "${state_directory_before}" &&
    -f ${STATE_SENTINEL} && ! -L ${STATE_SENTINEL} &&
    $(sha256sum "${STATE_SENTINEL}" | awk 'NR == 1 { print $1 }') == "${state_sentinel_before}" ]] || \
    die "${scenario} changed the existing state directory"
  [[ $(
    find "${STATE_DIR}" -mindepth 1 -maxdepth 1 -printf '%P:%y:%u:%g:%m\n' |
      LC_ALL=C sort |
      sha256sum |
      awk 'NR == 1 { print $1 }'
  ) == "${state_listing_before}" ]] || \
    die "${scenario} changed the existing state directory content"
}

assert_existing_archive_unchanged() {
  local scenario=$1
  [[ -d ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} &&
    $(stat -c '%d:%i:%u:%g:%a' -- "${ARCHIVE_DIR}") == "${archive_directory_before}" &&
    -f ${ARCHIVE_SENTINEL} && ! -L ${ARCHIVE_SENTINEL} &&
    $(sha256sum "${ARCHIVE_SENTINEL}" | awk 'NR == 1 { print $1 }') == "${archive_sentinel_before}" ]] || \
    die "${scenario} changed the existing archive directory"
  [[ $(
    find "${ARCHIVE_DIR}" -mindepth 1 -maxdepth 1 -printf '%P:%y:%u:%g:%m\n' |
      LC_ALL=C sort |
      sha256sum |
      awk 'NR == 1 { print $1 }'
  ) == "${archive_listing_before}" ]] || \
    die "${scenario} changed the existing archive directory content"
}

chmod 0666 "${UNIT_PATH}"
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/state-preflight-failure.out" 2>&1
state_preflight_failure_status=$?
set -e
adopt_installer_paths
[[ ${state_preflight_failure_status} -ne 0 ]] || \
  die "unsafe systemd unit unexpectedly passed the later preflight"
grep -F -- "existing systemd unit must not be group/other writable" \
  "${WORK_DIR}/state-preflight-failure.out" >/dev/null || \
  die "unsafe systemd unit did not reach the later preflight"
chmod 0644 "${UNIT_PATH}"
assert_existing_state_unchanged "state preflight failure"
assert_existing_archive_unchanged "archive preflight failure"

env_before="$(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }')"
config_before="$(sha256sum "${CONFIG_PATH}" | awk 'NR == 1 { print $1 }')"
unit_before="$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
legacy_runtime_unit_before="$(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
install -d -o root -g root -m 0700 /opt/autostream
shared_managed_parent_before="$(stat -c '%d:%i:%u:%g:%a' -- /opt/autostream)"
legacy_public_binary_metadata_before="$(
  stat -c '%d:%i:%u:%g:%a' -- "${PUBLIC_BINARY}"
)"
legacy_public_binary_sha_before="$(
  sha256sum "${PUBLIC_BINARY}" | awk 'NR == 1 { print $1 }'
)"
legacy_public_alias_metadata_before="$(
  stat -c '%d:%i:%u:%g:%a' -- "${PUBLIC_ALIAS}"
)"
legacy_public_alias_sha_before="$(
  sha256sum "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }'
)"
preexisting_backup_dir="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
install_backup_root_owned=true
install -d -o root -g root -m 0700 "${preexisting_backup_dir}"
install -o root -g root -m 0500 "${PUBLIC_BINARY}" \
  "${preexisting_backup_dir}/autostream-encoder-recorder"
install -o root -g root -m 0500 "${PUBLIC_ALIAS}" \
  "${preexisting_backup_dir}/encoder-recorder"
[[ $(stat -c '%u:%g:%a' -- "${preexisting_backup_dir}") == "0:0:700" ]] || \
  die "pre-existing backup directory fixture is not root-only"
[[ $(stat -c '%u:%g:%a' -- \
  "${preexisting_backup_dir}/autostream-encoder-recorder") == "0:0:500" ]] || \
  die "pre-existing canonical backup fixture is not root-only"
[[ $(stat -c '%u:%g:%a' -- \
  "${preexisting_backup_dir}/encoder-recorder") == "0:0:500" ]] || \
  die "pre-existing alias backup fixture is not root-only"
preexisting_backup_dir_metadata_before="$(
  stat -c '%d:%i:%u:%g:%a' -- "${preexisting_backup_dir}"
)"
preexisting_backup_binary_metadata_before="$(
  stat -c '%d:%i:%u:%g:%a' -- \
    "${preexisting_backup_dir}/autostream-encoder-recorder"
)"
preexisting_backup_binary_sha_before="$(
  sha256sum "${preexisting_backup_dir}/autostream-encoder-recorder" |
    awk 'NR == 1 { print $1 }'
)"
preexisting_backup_alias_metadata_before="$(
  stat -c '%d:%i:%u:%g:%a' -- "${preexisting_backup_dir}/encoder-recorder"
)"
preexisting_backup_alias_sha_before="$(
  sha256sum "${preexisting_backup_dir}/encoder-recorder" |
    awk 'NR == 1 { print $1 }'
)"

assert_preexisting_backups_unchanged() {
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- "${preexisting_backup_dir}")" == \
    "${preexisting_backup_dir_metadata_before}" ]] || \
    die "pre-existing backup directory metadata changed"
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- \
    "${preexisting_backup_dir}/autostream-encoder-recorder")" == \
    "${preexisting_backup_binary_metadata_before}" ]] || \
    die "pre-existing canonical backup inode or metadata changed"
  [[ "$(sha256sum "${preexisting_backup_dir}/autostream-encoder-recorder" |
    awk 'NR == 1 { print $1 }')" == \
    "${preexisting_backup_binary_sha_before}" ]] || \
    die "pre-existing canonical backup content changed"
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- \
    "${preexisting_backup_dir}/encoder-recorder")" == \
    "${preexisting_backup_alias_metadata_before}" ]] || \
    die "pre-existing alias backup inode or metadata changed"
  [[ "$(sha256sum "${preexisting_backup_dir}/encoder-recorder" |
    awk 'NR == 1 { print $1 }')" == \
    "${preexisting_backup_alias_sha_before}" ]] || \
    die "pre-existing alias backup content changed"
}

assert_legacy_public_paths_unchanged() {
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- "${PUBLIC_BINARY}")" == \
    "${legacy_public_binary_metadata_before}" ]] || \
    die "failed migration changed the legacy canonical binary inode or metadata"
  [[ "$(sha256sum "${PUBLIC_BINARY}" | awk 'NR == 1 { print $1 }')" == \
    "${legacy_public_binary_sha_before}" ]] || \
    die "failed migration changed the legacy canonical binary content"
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- "${PUBLIC_ALIAS}")" == \
    "${legacy_public_alias_metadata_before}" ]] || \
    die "failed migration changed the legacy alias inode or metadata"
  [[ "$(sha256sum "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }')" == \
    "${legacy_public_alias_sha_before}" ]] || \
    die "failed migration changed the legacy alias content"
  [[ "${legacy_public_binary_sha_before}" == \
    "${preexisting_backup_binary_sha_before}" ]] || \
    die "pre-existing canonical backup was not bound to the live legacy binary"
  [[ "${legacy_public_alias_sha_before}" == \
    "${preexisting_backup_alias_sha_before}" ]] || \
    die "pre-existing alias backup was not bound to the live legacy binary"
}

assert_shared_managed_parent_unchanged() {
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- /opt/autostream)" == \
    "${shared_managed_parent_before}" ]] || \
    die "failed migration did not restore the shared managed parent exactly"
}

legacy_fragment_before="$(systemctl show --property FragmentPath --value "${UNIT}")"
legacy_exec_start_before="$(systemctl show --property ExecStart --value "${UNIT}")"
legacy_user_before="$(systemctl show --property User --value "${UNIT}")"
[[ ${legacy_fragment_before} == "${RUNTIME_UNIT_PATH}" ]] || \
  die "legacy PID1 FragmentPath does not use the owned runtime unit"
[[ ${legacy_exec_start_before} == *"path=/usr/bin/sleep"* &&
  ${legacy_exec_start_before} == *"argv[]=/usr/bin/sleep infinity"* ]] || \
  die "legacy PID1 ExecStart is not the runtime shadow command"
[[ ${legacy_user_before} == "root" ]] || die "legacy PID1 User is not root"
assert_legacy_pid1_state "legacy baseline"

install -o root -g root -m 0755 /usr/bin/sync "${WORK_DIR}/real-sync"
printf '%s\n' \
  '#!/bin/sh' \
  'if [ "${1:-}" = "-f" ] && [ "${2:-}" = "/usr/local/bin" ]; then' \
  "  if [ ! -e '${WORK_DIR}/public-sync-failed' ]; then" \
  "    : > '${WORK_DIR}/public-sync-failed'" \
  '    exit 74' \
  '  fi' \
  'fi' \
  "exec '${WORK_DIR}/real-sync' \"\$@\"" \
  > "${WORK_DIR}/fail-public-sync"
chmod 0755 "${WORK_DIR}/fail-public-sync"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${WORK_DIR}/fail-public-sync' /usr/bin/sync && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/public-sync-failure.out" 2>&1
public_sync_status=$?
set -e
adopt_installer_paths
[[ ${public_sync_status} -eq 74 ]] || die "public-link sync failure injection returned an unexpected status"
[[ -f ${WORK_DIR}/public-sync-failed ]] || die "public-link sync failure injection did not reach its shim"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current ]] || \
  die "public-link sync failure left current activated"
grep -Fx -- "${LEGACY_BINARY_CONTENT}" "${PUBLIC_BINARY}" >/dev/null || \
  die "public-link sync failure changed the legacy canonical binary"
grep -Fx -- "${LEGACY_ALIAS_CONTENT}" "${PUBLIC_ALIAS}" >/dev/null || \
  die "public-link sync failure changed the legacy alias"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "public-link sync failure changed the existing environment"
[[ $(sha256sum "${CONFIG_PATH}" | awk 'NR == 1 { print $1 }') == "${config_before}" ]] || \
  die "public-link sync failure changed config.yml"
[[ $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "public-link sync failure did not restore the systemd unit"
assert_existing_state_unchanged "activation failure"
assert_existing_archive_unchanged "archive activation failure"
assert_legacy_public_paths_unchanged
assert_preexisting_backups_unchanged
assert_shared_managed_parent_unchanged
assert_legacy_pid1_state "public-link sync failure"
assert_not_enabled

set +e
unshare --mount --propagation private bash -c \
  "mount -t tmpfs tmpfs /run/systemd && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/failed-install.out" 2>&1
failed_status=$?
set -e
adopt_installer_paths
[[ ${failed_status} -ne 0 ]] || die "daemon-reload failure injection unexpectedly succeeded"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current ]] || \
  die "failed migration left current activated"
[[ -f ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} ]] || \
  die "failed migration did not restore the legacy canonical binary"
[[ -f ${PUBLIC_ALIAS} && ! -L ${PUBLIC_ALIAS} ]] || \
  die "failed migration did not restore the legacy alias"
grep -Fx -- "${LEGACY_BINARY_CONTENT}" "${PUBLIC_BINARY}" >/dev/null || \
  die "failed migration changed the legacy canonical binary"
grep -Fx -- "${LEGACY_ALIAS_CONTENT}" "${PUBLIC_ALIAS}" >/dev/null || \
  die "failed migration changed the legacy alias"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "failed migration changed the existing environment"
[[ $(sha256sum "${CONFIG_PATH}" | awk 'NR == 1 { print $1 }') == "${config_before}" ]] || \
  die "failed migration changed config.yml"
[[ $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "failed migration did not restore the systemd unit"
assert_existing_state_unchanged "activation failure"
assert_existing_archive_unchanged "archive activation failure"
assert_legacy_public_paths_unchanged
assert_preexisting_backups_unchanged
assert_shared_managed_parent_unchanged
assert_legacy_pid1_state "daemon-reload failure"
assert_not_enabled

RECOVERY_PATH="$(
  sed -n \
    's/^install-autostream-encoder-recorder: root-only recovery evidence preserved at //p' \
    "${WORK_DIR}/failed-install.out" |
    tail -n 1
)"
[[ ${RECOVERY_PATH} == /var/tmp/autostream-encoder-recorder-install.* ]] || \
  die "failed rollback did not report a bounded recovery path"
[[ -d ${RECOVERY_PATH} && ! -L ${RECOVERY_PATH} ]] || \
  die "reported recovery path is missing or unsafe"
[[ $(stat -c '%U:%G:%a' -- "${RECOVERY_PATH}") == "root:root:700" ]] || \
  die "recovery path is not root-only"
recovery_path_owned=true
[[ -f ${RECOVERY_PATH}/unit.previous && -f ${RECOVERY_PATH}/recovery-state.txt ]] || \
  die "recovery evidence does not retain the previous unit and baseline metadata"
rm -rf -- "${RECOVERY_PATH}"
recovery_path_owned=false
RECOVERY_PATH=""

retry_backup_dir="${preexisting_backup_dir}"
assert_preexisting_backups_unchanged

set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/migration.out"
migration_status=$?
set -e
adopt_installer_paths
[[ ${migration_status} -eq 0 ]] || die "legacy migration installer invocation failed"
[[ $(stat -c '%U:%G:%a' -- /opt/autostream) == "root:root:755" ]] || \
  die "successful migration did not normalize the shared managed parent"
readonly RELEASE_DIR="${MANAGED_ROOT}/releases/${VERSION}-${archive_sha256:0:12}"
readonly INSTALL_BACKUP_DIR="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
[[ $(readlink -f -- "${MANAGED_ROOT}/current") == "${RELEASE_DIR}" ]] || \
  die "successful migration did not activate the verified release"
[[ $(readlink -f -- "${PUBLIC_BINARY}") == "${RELEASE_DIR}/bin/autostream-encoder-recorder" ]] || \
  die "canonical public link does not resolve to the verified release"
[[ $(readlink -f -- "${PUBLIC_ALIAS}") == "${RELEASE_DIR}/bin/autostream-encoder-recorder" ]] || \
  die "public alias does not resolve to the verified release"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "successful migration changed the existing environment"
[[ $(sha256sum "${CONFIG_PATH}" | awk 'NR == 1 { print $1 }') == "${config_before}" ]] || \
  die "successful migration changed config.yml"
[[ $(stat -c '%U:%G:%a' -- "${STATE_DIR}") == "autostream:autostream:750" &&
  $(sha256sum "${STATE_SENTINEL}" | awk 'NR == 1 { print $1 }') == "${state_sentinel_before}" ]] || \
  die "successful migration did not normalize state metadata while preserving content"
[[ $(stat -c '%U:%G:%a' -- "${ARCHIVE_DIR}") == "autostream:autostream:750" &&
  $(sha256sum "${ARCHIVE_SENTINEL}" | awk 'NR == 1 { print $1 }') == "${archive_sentinel_before}" ]] || \
  die "successful migration did not normalize archive metadata while preserving content"
grep -Fx -- "${LEGACY_BINARY_CONTENT}" \
  "${INSTALL_BACKUP_DIR}/autostream-encoder-recorder" >/dev/null || \
  die "successful migration did not retain the legacy canonical binary"
grep -Fx -- "${LEGACY_ALIAS_CONTENT}" \
  "${INSTALL_BACKUP_DIR}/encoder-recorder" >/dev/null || \
  die "successful migration did not retain the legacy alias"
[[ $(stat -c '%U:%G:%a' -- /var/backups/autostream) == "root:root:700" ]] || \
  die "installer backup parent is not root-only"
grep -F -- "sudo systemctl restart ${UNIT}" "${WORK_DIR}/migration.out" >/dev/null || \
  die "active migration did not print the explicit restart command"
runtime_race_fragment_before="$(systemctl show --property FragmentPath --value "${UNIT}")"
runtime_race_exec_start_before="$(systemctl show --property ExecStart --value "${UNIT}")"
runtime_race_user_before="$(systemctl show --property User --value "${UNIT}")"
runtime_race_pid_before="$(systemctl show --property MainPID --value "${UNIT}")"
runtime_race_enabled_before="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"
runtime_sync_precommit_hook=replace_runtime_unit_for_precommit_probe
set +e
sync_private_unit_to_runtime
runtime_race_status=$?
set -e
runtime_sync_precommit_hook=""
[[ ${runtime_race_status} -eq 75 ]] || \
  die "runtime precommit race unexpectedly committed"
[[ ${runtime_race_active} == true ]] || \
  die "runtime precommit race did not retain recovery ownership"
[[ $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == \
  "${runtime_race_foreign_identity}" ]] || \
  die "precommit race changed the foreign runtime unit inode"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "${runtime_race_foreign_hash}" ]] || \
  die "precommit race changed the foreign runtime unit hash"
[[ $(systemctl show --property FragmentPath --value "${UNIT}") == \
  "${runtime_race_fragment_before}" ]] || \
  die "precommit race changed PID1 FragmentPath"
[[ $(systemctl show --property ExecStart --value "${UNIT}") == \
  "${runtime_race_exec_start_before}" ]] || \
  die "precommit race changed PID1 ExecStart"
[[ $(systemctl show --property User --value "${UNIT}") == \
  "${runtime_race_user_before}" ]] || \
  die "precommit race changed PID1 User"
[[ $(systemctl show --property MainPID --value "${UNIT}") == \
  "${runtime_race_pid_before}" ]] || \
  die "precommit race changed PID1 MainPID"
[[ $(systemctl is-enabled "${UNIT}" 2>/dev/null || true) == \
  "${runtime_race_enabled_before}" ]] || \
  die "precommit race changed the enabled state"
kill -0 "${old_pid}" || die "precommit race stopped the legacy process"
restore_runtime_sync_race || die "could not restore the owned runtime unit after the race probe"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "${legacy_runtime_unit_before}" ]] || \
  die "race probe did not restore the legacy runtime unit"
sync_private_unit_to_runtime
assert_migrated_pid1_state "successful migration"
assert_not_enabled

idempotent_runtime_identity_before="${runtime_unit_identity}"
idempotent_runtime_hash_before="$(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/idempotent.out"
idempotent_status=$?
set -e
adopt_installer_paths
[[ ${idempotent_status} -eq 0 ]] || die "idempotent installer invocation failed"
assert_migrated_pid1_state "idempotent reinstall"
[[ ${runtime_unit_identity} == "${idempotent_runtime_identity_before}" &&
  $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == "${idempotent_runtime_identity_before}" ]] || \
  die "idempotent reinstall changed the managed runtime unit inode"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "${idempotent_runtime_hash_before}" ]] || \
  die "idempotent reinstall changed the managed runtime unit hash"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "idempotent reinstall changed the existing environment"
[[ $(sha256sum "${CONFIG_PATH}" | awk 'NR == 1 { print $1 }') == "${config_before}" ]] || \
  die "idempotent reinstall changed config.yml"
assert_not_enabled

chown -h autostream:autostream "${MANAGED_ROOT}/current"
set +e
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
  > "${WORK_DIR}/malformed-current.out" 2>&1
malformed_current_status=$?
set -e
adopt_installer_paths
[[ ${malformed_current_status} -ne 0 ]] || \
  die "installer accepted a non-root-owned managed current link"
grep -F -- "managed current link must be owned by root:root" \
  "${WORK_DIR}/malformed-current.out" >/dev/null || \
  die "malformed current link did not fail closed with the expected message"
chown -h root:root "${MANAGED_ROOT}/current"
assert_migrated_pid1_state "malformed current validation"

[[ ${target_lock_owned} == true ]] || die "fixture does not own the updater target lock"
target_contention_lock_before="$(
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)"
(
  exec 7<>"${TARGET_LOCK}"
  flock -n 7 || die "test could not acquire the updater target lock"
  set +e
  "${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
    > "${WORK_DIR}/contention.out" 2>&1
  contention_status=$?
  set -e
  [[ ${contention_status} -ne 0 ]] || die "installer ignored updater lock contention"
)
[[ "$(
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${target_contention_lock_before}" ]] || \
  die "lock contention replaced or truncated the permanent updater lock"
grep -F -- "another privileged update is already active for ${UNIT}" \
  "${WORK_DIR}/contention.out" >/dev/null || \
  die "lock contention did not fail with the expected message"
assert_migrated_pid1_state "lock contention"

printf '%s\n' "Encoder Recorder installer integration scenarios passed."
