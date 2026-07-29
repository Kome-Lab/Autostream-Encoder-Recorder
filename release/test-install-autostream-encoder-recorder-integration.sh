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
  systemctl is-enabled --quiet "${UNIT}" &&
    die "installer unexpectedly enabled ${UNIT}"
  return 0
}

[[ ${EUID} -eq 0 ]] || die "must run as root"
[[ $(uname -m) == "x86_64" ]] || die "this integration fixture requires an amd64 Linux runner"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
[[ ${SCRIPT_DIR} == /* && -d ${SCRIPT_DIR} ]] || die "could not resolve the fixture directory"
readonly SCRIPT_DIR
readonly INSTALLER_SOURCE="${SCRIPT_DIR}/install-autostream-encoder-recorder"
readonly VERSION="v9.9.9"
readonly ARTIFACT_ID="autostream-encoder-recorder_${VERSION}_linux_amd64"
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
readonly UNIT="autostream-encoder-recorder.service"
readonly UNIT_PATH="/etc/systemd/system/${UNIT}"
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
readonly LEGACY_UNIT_CONTENT="encoder-recorder-installer-integration-legacy-unit"
readonly LEGACY_BINARY_CONTENT="encoder-recorder-installer-integration-legacy-binary"
readonly LEGACY_ALIAS_CONTENT="encoder-recorder-installer-integration-legacy-alias"
readonly LEGACY_ENV_CONTENT="ENCODER_RECORDER_INSTALLER_INTEGRATION_ENV=preserve-exactly"
readonly LEGACY_CONFIG_CONTENT="encoder-recorder-installer-integration-config-preserve-exactly"

created_autostream_user=false
old_pid=""
original_usr_local_bin_mode=""
usr_local_bin_mode_normalized=false

cleanup() {
  local exit_code=$?
  set +e
  systemctl stop "${UNIT}" >/dev/null 2>&1
  systemctl disable "${UNIT}" >/dev/null 2>&1
  rm -f -- "${UNIT_PATH}"
  systemctl daemon-reload >/dev/null 2>&1
  if [[ -n ${old_pid} ]]; then
    kill "${old_pid}" >/dev/null 2>&1
  fi
  rm -f -- "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}" "${ENV_PATH}" "${TARGET_LOCK}"
  rm -rf -- \
    "${CONFIG_DIR}" \
    "${STATE_DIR}" \
    "${ARCHIVE_DIR}" \
    "${MANAGED_ROOT}" \
    "${INSTALL_BACKUP_ROOT}" \
    "${WORK_DIR}"
  rmdir \
    /unpack \
    /run/autostream-updater \
    /var/backups/autostream/install-migrations \
    /var/backups/autostream \
    /var/lib/autostream \
    /opt/autostream \
    /etc/autostream >/dev/null 2>&1
  if [[ ${created_autostream_user} == true ]]; then
    userdel autostream >/dev/null 2>&1
    groupdel autostream >/dev/null 2>&1
  fi
  if [[ ${usr_local_bin_mode_normalized} == true ]]; then
    if ! chmod "${original_usr_local_bin_mode}" /usr/local/bin ||
      [[ $(stat -c '%a' -- /usr/local/bin) != "${original_usr_local_bin_mode}" ]]; then
      printf '%s\n' "encoder-recorder installer integration test: failed to restore /usr/local/bin mode" >&2
      exit_code=1
    fi
  fi
  exit "${exit_code}"
}
trap cleanup EXIT

for path in \
  "${UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${CONFIG_DIR}" \
  "${STATE_DIR}" \
  "${ARCHIVE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || die "runner is not clean at ${path}"
done
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "runner already has an autostream account"
fi
[[ ! -e /unpack && ! -L /unpack ]] || die "runner is not clean at /unpack"

[[ -d /usr/local/bin && ! -L /usr/local/bin &&
  $(readlink -f -- /usr/local/bin) == "/usr/local/bin" &&
  $(stat -c '%U:%G' -- /usr/local/bin) == "root:root" ]] || \
  die "runner /usr/local/bin boundary is unsafe"
original_usr_local_bin_mode="$(stat -c '%a' -- /usr/local/bin)"
[[ ${original_usr_local_bin_mode} =~ ^[0-7]{3,4}$ ]] || \
  die "runner /usr/local/bin mode is invalid"
(( (8#${original_usr_local_bin_mode} & 07000) == 0 )) || \
  die "runner /usr/local/bin has unexpected special mode bits"
if (( (8#${original_usr_local_bin_mode} & 0022) != 0 )); then
  printf 'Normalizing /usr/local/bin mode %s for the isolated integration fixture.\n' \
    "${original_usr_local_bin_mode}"
  chmod go-w /usr/local/bin
  usr_local_bin_mode_normalized=true
fi
normalized_usr_local_bin_mode="$(stat -c '%a' -- /usr/local/bin)"
(( (8#${normalized_usr_local_bin_mode} & 07022) == 0 )) || \
  die "runner /usr/local/bin could not be normalized safely"

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
  printf '%s\n' 'commit: integration-test'
  printf '%s\n' 'build_date: integration-test'
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

(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
(
  cd -- "${ARTIFACTS_DIR}"
  sha256sum "${ARTIFACT_ID}.tar.gz" > "${ARTIFACT_ID}.tar.gz.sha256"
)
archive_sha256="$(sha256sum "${ARCHIVE}" | awk 'NR == 1 { print $1 }')"
archive_size="$(stat -c %s "${ARCHIVE}")"
jq -n \
  --arg version "${VERSION}" \
  --arg name "${ARTIFACT_ID}.tar.gz" \
  --arg sha256 "${archive_sha256}" \
  --argjson size "${archive_size}" \
  '{
    schema_version: 1,
    release_id: $version,
    channel: "host",
    published_at: "2026-01-01T00:00:00Z",
    minimum_agent_version: "v1.0.0",
    components: [{
      service: "encoder-recorder",
      source_version: $version,
      commit: ("0" * 40),
      rollback_compatible: true,
      database_schema: "none",
      artifacts: [
        {
          os: "linux",
          arch: "amd64",
          name: $name,
          sha256: $sha256,
          size: $size
        },
        {
          os: "linux",
          arch: "arm64",
          name: ("autostream-encoder-recorder_" + $version + "_linux_arm64.tar.gz"),
          sha256: ("0" * 64),
          size: 1
        }
      ]
    }]
  }' > "${ARTIFACTS_DIR}/release-manifest.json"
(
  cd -- "${ARTIFACTS_DIR}"
  sha256sum release-manifest.json > release-manifest.json.sha256
)

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
[[ ! -e /unpack && ! -L /unpack ]] || die "mktemp failure created a root-level /unpack path"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "mktemp failure mutated the service account"
fi

created_autostream_user=true
"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/fresh.out"
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

rm -f -- "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}" "${ENV_PATH}" "${UNIT_PATH}"
rm -rf -- "${STATE_DIR}" "${ARCHIVE_DIR}" "${MANAGED_ROOT}" "${INSTALL_BACKUP_ROOT}"
systemctl daemon-reload

install -d -o autostream -g autostream -m 0750 "${STATE_DIR}" "${ARCHIVE_DIR}"
install -d -o root -g root -m 0750 /etc/autostream
printf '%s\n' "${LEGACY_ENV_CONTENT}" > "${ENV_PATH}"
chmod 0640 "${ENV_PATH}"
install -d -o root -g root -m 0700 "${CONFIG_DIR}"
printf '%s\n' "${LEGACY_CONFIG_CONTENT}" > "${CONFIG_PATH}"
chmod 0600 "${CONFIG_PATH}"
printf '%s\n' "${LEGACY_BINARY_CONTENT}" > "${PUBLIC_BINARY}"
chmod 0755 "${PUBLIC_BINARY}"
printf '%s\n' "${LEGACY_ALIAS_CONTENT}" > "${PUBLIC_ALIAS}"
chmod 0755 "${PUBLIC_ALIAS}"
cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=${LEGACY_UNIT_CONTENT}

[Service]
Type=simple
ExecStart=/usr/bin/sleep infinity
Restart=on-failure
EOF
chmod 0644 "${UNIT_PATH}"
systemctl daemon-reload
systemctl start "${UNIT}"
old_pid="$(systemctl show --property MainPID --value "${UNIT}")"
[[ ${old_pid} =~ ^[1-9][0-9]*$ ]] || die "legacy service did not start"
kill -0 "${old_pid}" || die "legacy service PID is not alive"
assert_not_enabled

env_before="$(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }')"
config_before="$(sha256sum "${CONFIG_PATH}" | awk 'NR == 1 { print $1 }')"
unit_before="$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')"

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
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "public-link sync failure replaced the running legacy process"
assert_not_enabled

set +e
unshare --mount --propagation private bash -c \
  "mount -t tmpfs tmpfs /run/systemd && '${EXTRACTED_ROOT}/install-autostream-encoder-recorder'" \
  > "${WORK_DIR}/failed-install.out" 2>&1
failed_status=$?
set -e
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
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "failed migration replaced the running legacy process"
kill -0 "${old_pid}" || die "failed migration stopped the legacy process"
assert_not_enabled

recovery_path="$(
  sed -n \
    's/^install-autostream-encoder-recorder: root-only recovery evidence preserved at //p' \
    "${WORK_DIR}/failed-install.out" |
    tail -n 1
)"
[[ ${recovery_path} == /var/tmp/autostream-encoder-recorder-install.* ]] || \
  die "failed rollback did not report a bounded recovery path"
[[ -d ${recovery_path} && ! -L ${recovery_path} ]] || \
  die "reported recovery path is missing or unsafe"
[[ $(stat -c '%U:%G:%a' -- "${recovery_path}") == "root:root:700" ]] || \
  die "recovery path is not root-only"
[[ -f ${recovery_path}/unit.previous && -f ${recovery_path}/recovery-state.txt ]] || \
  die "recovery evidence does not retain the previous unit and baseline metadata"
rm -rf -- "${recovery_path}"

retry_backup_dir="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
install -d -o root -g root -m 0700 "${retry_backup_dir}"
install -o root -g root -m 0500 "${PUBLIC_BINARY}" \
  "${retry_backup_dir}/autostream-encoder-recorder"
install -o root -g root -m 0500 "${PUBLIC_ALIAS}" \
  "${retry_backup_dir}/encoder-recorder"

"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/migration.out"
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
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "successful migration replaced the running legacy process"
kill -0 "${old_pid}" || die "successful migration stopped the legacy process"
assert_not_enabled

"${EXTRACTED_ROOT}/install-autostream-encoder-recorder" > "${WORK_DIR}/idempotent.out"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "idempotent reinstall replaced the running legacy process"
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
[[ ${malformed_current_status} -ne 0 ]] || \
  die "installer accepted a non-root-owned managed current link"
grep -F -- "managed current link must be owned by root:root" \
  "${WORK_DIR}/malformed-current.out" >/dev/null || \
  die "malformed current link did not fail closed with the expected message"
chown -h root:root "${MANAGED_ROOT}/current"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "malformed current validation changed the running legacy process"

(
  exec 8>"${TARGET_LOCK}"
  flock -n 8 || die "test could not acquire the updater target lock"
  set +e
  "${EXTRACTED_ROOT}/install-autostream-encoder-recorder" \
    > "${WORK_DIR}/contention.out" 2>&1
  contention_status=$?
  set -e
  [[ ${contention_status} -ne 0 ]] || die "installer ignored updater lock contention"
)
grep -F -- "another privileged update is already active for ${UNIT}" \
  "${WORK_DIR}/contention.out" >/dev/null || \
  die "lock contention did not fail with the expected message"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "lock contention changed the running legacy process"

printf '%s\n' "Encoder Recorder installer integration scenarios passed."
