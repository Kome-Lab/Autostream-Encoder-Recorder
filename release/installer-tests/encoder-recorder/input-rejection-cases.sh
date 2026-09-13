
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
