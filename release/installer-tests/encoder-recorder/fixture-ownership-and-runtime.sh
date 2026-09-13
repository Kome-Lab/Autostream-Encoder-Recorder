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
    userdel autostream-install-rollback >/dev/null 2>&1
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
if getent passwd autostream-install-rollback >/dev/null 2>&1 ||
  getent group autostream-install-rollback >/dev/null 2>&1; then
  die "runner already has the reserved service-account rollback login"
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
