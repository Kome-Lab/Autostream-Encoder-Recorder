
install -o root -g root -m 0755 /usr/bin/sync "${WORK_DIR}/real-sync"
printf '%s\n' \
  '#!/bin/sh' \
  '{' \
  '  printf "argc=%s" "$#"' \
  '  for public_sync_arg in "$@"; do' \
  '    printf " <%s>" "${public_sync_arg}"' \
  '  done' \
  '  printf "\n"' \
  "} >> '${WORK_DIR}/public-sync-argv.trace'" \
  'if [ "${1:-}" = "-f" ] && [ "${2:-}" = "/usr/local/bin" ] &&' \
  '  [ -L /usr/local/bin/encoder-recorder ] &&' \
  '  [ "$(readlink -- /usr/local/bin/encoder-recorder)" = "/usr/local/bin/autostream-encoder-recorder" ]; then' \
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
if [[ ${public_sync_status} -ne 74 ]]; then
  printf 'encoder-recorder installer integration test: public-link sync failure actual status: %s\n' \
    "${public_sync_status}" >&2
  if [[ -f ${WORK_DIR}/public-sync-failed && ! -L ${WORK_DIR}/public-sync-failed ]]; then
    printf '%s\n' \
      'encoder-recorder installer integration test: public-link sync failure shim marker: reached' >&2
  else
    printf '%s\n' \
      'encoder-recorder installer integration test: public-link sync failure shim marker: not reached' >&2
  fi
  printf '%s\n' \
    'encoder-recorder installer integration test: public-link sync failure shim argv trace:' >&2
  if [[ -f ${WORK_DIR}/public-sync-argv.trace && ! -L ${WORK_DIR}/public-sync-argv.trace ]]; then
    cat -- "${WORK_DIR}/public-sync-argv.trace" >&2 || \
      printf '%s\n' '<argv trace unreadable>' >&2
  else
    printf '%s\n' '<argv trace missing>' >&2
  fi
  printf '%s\n' \
    'encoder-recorder installer integration test: public-link sync failure captured installer output:' >&2
  if [[ -f ${WORK_DIR}/public-sync-failure.out && ! -L ${WORK_DIR}/public-sync-failure.out ]]; then
    cat -- "${WORK_DIR}/public-sync-failure.out" >&2 || \
      printf '%s\n' '<captured installer output unreadable>' >&2
  else
    printf '%s\n' '<captured installer output missing>' >&2
  fi
fi
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
assert_no_public_rollback_anchors "public-link sync failure"
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
assert_no_public_rollback_anchors "daemon-reload failure"
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
