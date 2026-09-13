
copy_stable_source() {
  local source=$1
  local destination=$2
  local identity
  local digest
  identity="$(stat -c '%d:%i:%s:%Y:%f' -- "${source}")"
  digest="$(sha256sum -- "${source}" | awk 'NR == 1 { print $1 }')"
  install -o root -g root -m 0600 "${source}" "${destination}"
  [[ -f ${source} && ! -L ${source} ]] || die "release source changed while being staged: ${source}"
  [[ $(stat -c '%d:%i:%s:%Y:%f' -- "${source}") == "${identity}" ]] || \
    die "release source identity changed while being staged: ${source}"
  [[ $(sha256sum -- "${source}" | awk 'NR == 1 { print $1 }') == "${digest}" ]] || \
    die "release source content changed while being staged: ${source}"
  [[ $(sha256sum -- "${destination}" | awk 'NR == 1 { print $1 }') == "${digest}" ]] || \
    die "staged release file differs from its source: ${source}"
}

copy_stable_source "${ARCHIVE_SOURCE}" "${INPUT_STAGE}/${ARCHIVE_NAME}"

ARTIFACT_SHA256="$(
  sha256sum -- "${INPUT_STAGE}/${ARCHIVE_NAME}" | awk 'NR == 1 { print $1 }'
)"
[[ ${ARTIFACT_SHA256} =~ ^[0-9a-f]{64}$ ]] || die "could not calculate the artifact SHA-256"
readonly ARTIFACT_SHA256
readonly LEGACY_BACKUP_DIR="${BACKUP_DIR}/${VERSION}-${ARTIFACT_SHA256:0:12}"
ARTIFACT_SIZE="$(stat -c %s -- "${INPUT_STAGE}/${ARCHIVE_NAME}")"
[[ ${ARTIFACT_SIZE} == "${ARCHIVE_SOURCE_SIZE}" ]] || \
  die "staged release archive size differs from its verified source"
readonly ARTIFACT_SIZE

tar -tzf "${INPUT_STAGE}/${ARCHIVE_NAME}" > "${INPUT_STAGE}/archive.list"
[[ -s ${INPUT_STAGE}/archive.list ]] || die "release archive is empty"
while IFS= read -r entry; do
  [[ ${entry} == "${ARTIFACT_ID}" || ${entry} == "${ARTIFACT_ID}/" || ${entry} == "${ARTIFACT_ID}/"* ]] || \
    die "release archive contains a path outside ${ARTIFACT_ID}: ${entry}"
  [[ ${entry} != /* &&
    ${entry} != *\\* &&
    ${entry} != ../* &&
    ${entry} != *"/../"* &&
    ${entry} != *"/.." &&
    ${entry} != *"/./"* &&
    ${entry} != *"/." &&
    ${entry} != *"//"* ]] || \
    die "release archive contains an unsafe path: ${entry}"
done < "${INPUT_STAGE}/archive.list"
duplicate_paths="$(
  awk '{ sub(/\/$/, ""); print }' "${INPUT_STAGE}/archive.list" |
    LC_ALL=C sort |
    uniq -d
)"
[[ -z ${duplicate_paths} ]] || die "release archive contains duplicate paths"
while IFS= read -r entry; do
  case "${entry:0:1}" in
    -|d) ;;
    *) die "release archive contains a link or non-regular entry" ;;
  esac
done < <(tar -tvzf "${INPUT_STAGE}/${ARCHIVE_NAME}")

tar --no-same-owner --no-same-permissions \
  -xzf "${INPUT_STAGE}/${ARCHIVE_NAME}" \
  -C "${UNPACK_DIR}"
readonly EXTRACTED_ROOT="${UNPACK_DIR}/${ARTIFACT_ID}"
[[ -d ${EXTRACTED_ROOT} && ! -L ${EXTRACTED_ROOT} ]] || die "release archive root is missing or unsafe"
[[ -z $(find "${UNPACK_DIR}" -mindepth 1 -maxdepth 1 ! -name "${ARTIFACT_ID}" -print -quit) ]] || \
  die "release archive contains more than one top-level path"
[[ -z $(find "${EXTRACTED_ROOT}" -type l -print -quit) ]] || die "release archive must not contain symlinks"
[[ -z $(find "${EXTRACTED_ROOT}" ! -type f ! -type d -print -quit) ]] || \
  die "release archive must contain only regular files and directories"
[[ -f ${EXTRACTED_ROOT}/checksums.txt && ! -L ${EXTRACTED_ROOT}/checksums.txt ]] || \
  die "release checksums.txt is missing or unsafe"

declare -A checksum_paths=()
while IFS= read -r checksum_entry; do
  [[ ${checksum_entry} =~ ^[0-9a-f]{64}\ \ (.+)$ ]] || die "inner checksum entry has an invalid format"
  checked_path="${BASH_REMATCH[1]}"
  [[ ${checked_path} == ./* ]] || die "inner checksum entry must use a ./ relative path"
  normalized_checked_path="${checked_path#./}"
  [[ -n ${normalized_checked_path} &&
    ${normalized_checked_path} != /* &&
    ${normalized_checked_path} != ./* &&
    ${normalized_checked_path} != *\\* &&
    ${normalized_checked_path} != ../* &&
    ${normalized_checked_path} != *"/../"* &&
    ${normalized_checked_path} != *"/.." &&
    ${normalized_checked_path} != *"/./"* &&
    ${normalized_checked_path} != *"/." &&
    ${normalized_checked_path} != *"//"* &&
    ${normalized_checked_path} != "checksums.txt" ]] || die "inner checksum entry has an unsafe path"
  [[ -z ${checksum_paths["${normalized_checked_path}"]+present} ]] || \
    die "inner checksum entry is duplicated: ${checked_path}"
  checksum_paths["${normalized_checked_path}"]=present
  [[ -f ${EXTRACTED_ROOT}/${checked_path} && ! -L ${EXTRACTED_ROOT}/${checked_path} ]] || \
    die "inner checksum entry does not name a regular file: ${checked_path}"
done < "${EXTRACTED_ROOT}/checksums.txt"
while IFS= read -r extracted_path; do
  extracted_relative="${extracted_path#"${EXTRACTED_ROOT}/"}"
  [[ ${extracted_relative} == "checksums.txt" ]] && continue
  [[ -n ${checksum_paths["${extracted_relative}"]+present} ]] || \
    die "release file is not covered by checksums.txt: ${extracted_relative}"
done < <(find "${EXTRACTED_ROOT}" -type f -print)
(cd -- "${EXTRACTED_ROOT}" && sha256sum --check --strict checksums.txt)

for required in \
  "bin/autostream-encoder-recorder" \
  "bin/encoder-recorder" \
  ".env.example" \
  "artifact-manifest.json" \
  "systemd/autostream-encoder-recorder.service.example" \
  "install-autostream-encoder-recorder"; do
  [[ -f ${EXTRACTED_ROOT}/${required} && ! -L ${EXTRACTED_ROOT}/${required} ]] || \
    die "required release file is missing or unsafe: ${required}"
done

jq -e \
  --arg component "${COMPONENT_NAME}" \
  --arg version "${VERSION}" \
  --arg arch "${ARCH}" \
  --arg archive_name "${ARCHIVE_NAME}" \
  --arg archive_root "${ARTIFACT_ID}" \
  '((keys | sort) ==
     ["archive", "build_date", "commit", "compatibility", "component", "platform", "schema_version", "source_version"]) and
   (.schema_version == 1) and
   (.component == $component) and
   (.source_version == $version) and
   (.commit | type == "string" and test("^[0-9a-f]{40}$")) and
   (.build_date | type == "string" and
     test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
   (.platform | type == "object") and
   ((.platform | keys | sort) == ["arch", "os"]) and
   (.platform.os == "linux") and
   (.platform.arch == $arch) and
   (.archive | type == "object") and
   ((.archive | keys | sort) == ["name", "root"]) and
   (.archive.name == $archive_name) and
   (.archive.root == $archive_root) and
   (.compatibility | type == "object") and
   ((.compatibility | keys | sort) ==
     ["database_schema", "minimum_agent_version", "minimum_panel_version", "rollback_compatible"]) and
   (.compatibility.minimum_agent_version == "v1.0.0") and
   (.compatibility.minimum_panel_version == null) and
   (.compatibility.rollback_compatible == true) and
   (.compatibility.database_schema == "none")' \
  "${EXTRACTED_ROOT}/artifact-manifest.json" >/dev/null || \
  die "artifact-manifest.json does not describe this exact Encoder Recorder artifact"

ARTIFACT_COMMIT="$(jq -r '.commit' "${EXTRACTED_ROOT}/artifact-manifest.json")"
readonly ARTIFACT_COMMIT
ARTIFACT_BUILD_DATE="$(jq -r '.build_date' "${EXTRACTED_ROOT}/artifact-manifest.json")"
readonly ARTIFACT_BUILD_DATE

[[ -x ${EXTRACTED_ROOT}/bin/autostream-encoder-recorder ]] || die "Encoder Recorder binary is not executable"
[[ -x ${EXTRACTED_ROOT}/bin/encoder-recorder ]] || die "Encoder Recorder compatibility binary is not executable"
cmp -s \
  "${EXTRACTED_ROOT}/bin/autostream-encoder-recorder" \
  "${EXTRACTED_ROOT}/bin/encoder-recorder" || die "Encoder Recorder compatibility binary differs from the canonical binary"
version_output="$("${EXTRACTED_ROOT}/bin/autostream-encoder-recorder" --version)"
expected_version_output="$(
  printf 'autostream-encoder-recorder %s\ncommit: %s\nbuild_date: %s' \
    "${VERSION}" "${ARTIFACT_COMMIT}" "${ARTIFACT_BUILD_DATE}"
)"
[[ ${version_output} == "${expected_version_output}" ]] || \
  die "Encoder Recorder binary identity does not exactly match artifact-manifest.json"
