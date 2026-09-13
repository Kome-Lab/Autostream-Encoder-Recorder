
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
preexisting_gshadow_digest="$(sha256sum -- /etc/gshadow | awk 'NR == 1 { print $1 }')"
[[ ${preexisting_gshadow_digest} =~ ^[0-9a-f]{64}$ ]] || \
  die "could not snapshot /etc/gshadow before the useradd TERM transaction"
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
if getent passwd autostream-install-rollback >/dev/null 2>&1 ||
  getent group autostream-install-rollback >/dev/null 2>&1; then
  die "useradd TERM transaction left the reserved rollback login"
fi
[[ $(getent group autostream) == "${preexisting_group_record}" ]] || \
  die "useradd TERM transaction changed the pre-existing service group"
[[ $(sha256sum -- /etc/gshadow | awk 'NR == 1 { print $1 }') == \
  "${preexisting_gshadow_digest}" ]] || \
  die "useradd TERM transaction changed /etc/gshadow"
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
