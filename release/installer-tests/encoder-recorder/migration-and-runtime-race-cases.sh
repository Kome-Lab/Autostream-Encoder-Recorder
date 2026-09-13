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
assert_no_public_rollback_anchors "successful migration"
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
assert_no_public_rollback_anchors "idempotent reinstall"
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
