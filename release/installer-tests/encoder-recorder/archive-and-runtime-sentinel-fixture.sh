
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
LoadCredential=node-listener.json:/opt/autostream/local-executor/ports/encoder-recorder.json
ExecStart=/usr/local/bin/autostream-encoder-recorder
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
printf '%s\n' 'AUTOSTREAM_NODE_CONFIG=/etc/autostream-encoder-recorder/config.yml' \
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
