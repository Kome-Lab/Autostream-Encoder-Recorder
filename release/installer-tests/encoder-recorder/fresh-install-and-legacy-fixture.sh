
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
legacy_public_binary_nlink_before="$(stat -c '%h' -- "${PUBLIC_BINARY}")"
legacy_public_alias_metadata_before="$(
  stat -c '%d:%i:%u:%g:%a' -- "${PUBLIC_ALIAS}"
)"
legacy_public_alias_sha_before="$(
  sha256sum "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }'
)"
legacy_public_alias_nlink_before="$(stat -c '%h' -- "${PUBLIC_ALIAS}")"
preexisting_backup_dir="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
install_backup_root_owned=true
install -d -o root -g root -m 0700 "${INSTALL_BACKUP_ROOT}"
install -d -o root -g root -m 0700 "${preexisting_backup_dir}"
install -o root -g root -m 0500 "${PUBLIC_BINARY}" \
  "${preexisting_backup_dir}/autostream-encoder-recorder"
install -o root -g root -m 0500 "${PUBLIC_ALIAS}" \
  "${preexisting_backup_dir}/encoder-recorder"
[[ $(stat -c '%u:%g:%a' -- "${INSTALL_BACKUP_ROOT}") == "0:0:700" ]] || \
  die "pre-existing backup root fixture is not root-only"
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
  [[ "$(stat -c '%h' -- "${PUBLIC_BINARY}")" == \
    "${legacy_public_binary_nlink_before}" ]] || \
    die "failed migration changed the legacy canonical binary link count"
  [[ "$(stat -c '%d:%i:%u:%g:%a' -- "${PUBLIC_ALIAS}")" == \
    "${legacy_public_alias_metadata_before}" ]] || \
    die "failed migration changed the legacy alias inode or metadata"
  [[ "$(sha256sum "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }')" == \
    "${legacy_public_alias_sha_before}" ]] || \
    die "failed migration changed the legacy alias content"
  [[ "$(stat -c '%h' -- "${PUBLIC_ALIAS}")" == \
    "${legacy_public_alias_nlink_before}" ]] || \
    die "failed migration changed the legacy alias link count"
  [[ "${legacy_public_binary_sha_before}" == \
    "${preexisting_backup_binary_sha_before}" ]] || \
    die "pre-existing canonical backup was not bound to the live legacy binary"
  [[ "${legacy_public_alias_sha_before}" == \
    "${preexisting_backup_alias_sha_before}" ]] || \
    die "pre-existing alias backup was not bound to the live legacy binary"
}

assert_no_public_rollback_anchors() {
  local scenario=$1
  local leftover=""
  leftover="$(
    find /usr/local/bin -mindepth 1 -maxdepth 1 \
      \( -name 'autostream-encoder-recorder.rollback-anchor.*' -o \
      -name 'encoder-recorder.rollback-anchor.*' \) -print -quit
  )"
  [[ -z ${leftover} ]] || \
    die "${scenario} left a public rollback anchor: ${leftover}"
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
