verify_release_checksum_inventory() {
  local release_dir=$1
  local checksum_entry
  local checked_path
  local release_file
  local relative_path
  local -A inventory=()
  [[ -f ${release_dir}/checksums.txt && ! -L ${release_dir}/checksums.txt ]] || \
    die "managed release checksums are missing or unsafe: ${release_dir}"
  while IFS= read -r checksum_entry; do
    [[ ${checksum_entry} =~ ^[0-9a-f]{64}\ \ (.+)$ ]] || \
      die "managed release checksum entry has an invalid format: ${release_dir}"
    checked_path="${BASH_REMATCH[1]}"
    checked_path="${checked_path#./}"
    [[ -n ${checked_path} &&
      ${checked_path} != /* &&
      ${checked_path} != ../* &&
      ${checked_path} != *"/../"* &&
      ${checked_path} != *"/.." &&
      ${checked_path} != *"/./"* &&
      ${checked_path} != *"/." &&
      ${checked_path} != *\\* ]] || \
      die "managed release checksum entry has an unsafe path: ${release_dir}"
    [[ -z ${inventory["${checked_path}"]+present} ]] || \
      die "managed release checksum path is duplicated: ${checked_path}"
    inventory["${checked_path}"]=present
    [[ -f ${release_dir}/${checked_path} && ! -L ${release_dir}/${checked_path} ]] || \
      die "managed release checksum entry does not name a regular file: ${checked_path}"
  done < "${release_dir}/checksums.txt"
  while IFS= read -r -d '' release_file; do
    relative_path="${release_file#"${release_dir}/"}"
    case "${relative_path}" in
      checksums.txt|.artifact-sha256|.version) continue ;;
    esac
    [[ -n ${inventory["${relative_path}"]+present} ]] || \
      die "managed release contains a file outside its checksum inventory: ${relative_path}"
  done < <(find "${release_dir}" -type f -print0)
}

verify_managed_release() {
  local release_dir=$1
  [[ -d ${release_dir} && ! -L ${release_dir} ]] || die "managed release is missing or unsafe: ${release_dir}"
  [[ -z $(find "${release_dir}" -type l -print -quit) ]] || die "managed release must not contain symlinks"
  [[ -z $(find "${release_dir}" ! -type d ! -type f -print -quit) ]] || \
    die "managed release must contain only directories and regular files"
  [[ -z $(find "${release_dir}" \( ! -user root -o ! -group root \) -print -quit) ]] || \
    die "managed release must be owned by root:root"
  [[ -f ${release_dir}/.artifact-sha256 && ! -L ${release_dir}/.artifact-sha256 ]] || \
    die "managed release digest marker is missing or unsafe"
  [[ -f ${release_dir}/.version && ! -L ${release_dir}/.version ]] || \
    die "managed release version marker is missing or unsafe"
  [[ $(wc -l < "${release_dir}/.artifact-sha256") -eq 1 ]] || die "managed release digest marker must contain one line"
  [[ $(wc -l < "${release_dir}/.version") -eq 1 ]] || die "managed release version marker must contain one line"
  [[ $(< "${release_dir}/.artifact-sha256") == "${ARTIFACT_SHA256}" ]] || \
    die "managed release digest marker differs from the verified artifact"
  [[ $(< "${release_dir}/.version") == "${VERSION}" ]] || \
    die "managed release version marker differs from the verified artifact"
  [[ $(stat -c '%a' -- "${release_dir}/.artifact-sha256") == "444" ]] || \
    die "managed release digest marker mode is not normalized"
  [[ $(stat -c '%a' -- "${release_dir}/.version") == "444" ]] || \
    die "managed release version marker mode is not normalized"
  cmp -s "${EXTRACTED_ROOT}/checksums.txt" "${release_dir}/checksums.txt" || \
    die "managed release checksums differ from the verified artifact"
  [[ -z $(find "${release_dir}" -type d ! -perm 0755 -print -quit) ]] || \
    die "managed release directory modes are not normalized"
  [[ $(stat -c '%a' -- "${release_dir}/bin/autostream-encoder-recorder") == "755" ]] || \
    die "managed canonical binary mode is not normalized"
  [[ $(stat -c '%a' -- "${release_dir}/bin/encoder-recorder") == "755" ]] || \
    die "managed compatibility binary mode is not normalized"
  [[ $(stat -c '%a' -- "${release_dir}/install-autostream-encoder-recorder") == "755" ]] || \
    die "managed installer mode is not normalized"
  [[ -z $(find "${release_dir}" -type f \
    ! -path "${release_dir}/bin/autostream-encoder-recorder" \
    ! -path "${release_dir}/bin/encoder-recorder" \
    ! -path "${release_dir}/install-autostream-encoder-recorder" \
    ! -path "${release_dir}/.artifact-sha256" \
    ! -path "${release_dir}/.version" \
    ! -perm 0644 -print -quit) ]] || die "managed release file modes are not normalized"
  while IFS= read -r managed_path; do
    managed_relative="${managed_path#"${release_dir}/"}"
    case "${managed_relative}" in
      checksums.txt|.artifact-sha256|.version) continue ;;
    esac
    [[ -n ${checksum_paths["${managed_relative}"]+present} ]] || \
      die "managed release contains a file outside the verified checksum inventory: ${managed_relative}"
  done < <(find "${release_dir}" -type f -print)
  verify_release_checksum_inventory "${release_dir}"
  (cd -- "${release_dir}" && sha256sum --check --strict checksums.txt)
}

if [[ -e ${RELEASE_DIR} || -L ${RELEASE_DIR} ]]; then
  verify_managed_release "${RELEASE_DIR}"
else
  create_registered_temporary_path managed_candidate directory \
    "${RELEASES_DIR}/.install-${VERSION}.XXXXXXXX" || \
    die "could not create managed release staging"
  managed_candidate_identity="${temporary_path_identity["${managed_candidate}"]}"
  cp -a --no-preserve=ownership -- "${EXTRACTED_ROOT}/." "${managed_candidate}/"
  chown -R root:root "${managed_candidate}"
  find "${managed_candidate}" -type d -exec chmod 0755 {} +
  find "${managed_candidate}" -type f -exec chmod 0644 {} +
  chmod 0755 \
    "${managed_candidate}/bin/autostream-encoder-recorder" \
    "${managed_candidate}/bin/encoder-recorder" \
    "${managed_candidate}/install-autostream-encoder-recorder"
  printf '%s\n' "${ARTIFACT_SHA256}" > "${managed_candidate}/.artifact-sha256"
  printf '%s\n' "${VERSION}" > "${managed_candidate}/.version"
  chmod 0444 "${managed_candidate}/.artifact-sha256" "${managed_candidate}/.version"
  (cd -- "${managed_candidate}" && sha256sum --check --strict checksums.txt)
  sync -f "${managed_candidate}"
  begin_installer_signal_transaction
  if ! mv -T -- "${managed_candidate}" "${RELEASE_DIR}"; then
    finish_installer_signal_transaction
    die "could not publish the managed release"
  fi
  forget_temporary_path "${managed_candidate}"
  managed_candidate=""
  managed_candidate_identity=""
  release_created=true
  if ! release_created_identity="$(stat -c '%d:%i' -- "${RELEASE_DIR}")"; then
    rollback_incomplete=true
    finish_installer_signal_transaction
    die "could not journal the published managed release identity"
  fi
  finish_installer_signal_transaction
  sync -f "${RELEASES_DIR}"
  verify_managed_release "${RELEASE_DIR}"
fi

managed_version_output="$(runuser -u autostream -- "${RELEASE_DIR}/bin/autostream-encoder-recorder" --version)"
[[ ${managed_version_output} == "${expected_version_output}" ]] || \
  die "managed Encoder Recorder binary is not runnable by autostream with the exact release identity"

validate_existing_current() {
  local current_release
  local marker_version
  local marker_digest
  local expected_basename
  local current_version_output
  local current_version_first_line
  [[ -L ${CURRENT_LINK} ]] || die "managed current path must be a symlink"
  [[ $(stat -c '%U:%G' -- "${CURRENT_LINK}") == "root:root" ]] || \
    die "managed current link must be owned by root:root"
  current_release="$(readlink -f -- "${CURRENT_LINK}")" || die "managed current link is dangling"
  [[ -d ${current_release} && ! -L ${current_release} ]] || die "managed current target is missing or unsafe"
  [[ $(dirname -- "${current_release}") == "${RELEASES_DIR}" ]] || die "managed current target escapes the releases directory"
  [[ -z $(find "${current_release}" -type l -print -quit) ]] || die "managed current release must not contain symlinks"
  [[ -z $(find "${current_release}" ! -type d ! -type f -print -quit) ]] || \
    die "managed current release must contain only directories and regular files"
  [[ -z $(find "${current_release}" \( ! -user root -o ! -group root \) -print -quit) ]] || \
    die "managed current release must be owned by root:root"
  [[ -z $(find "${current_release}" -type f -perm /0022 -print -quit) ]] || \
    die "managed current release contains a group/other-writable file"
  [[ -f ${current_release}/.artifact-sha256 && ! -L ${current_release}/.artifact-sha256 ]] || \
    die "managed current digest marker is missing or unsafe"
  [[ -f ${current_release}/.version && ! -L ${current_release}/.version ]] || \
    die "managed current version marker is missing or unsafe"
  [[ -f ${current_release}/checksums.txt && ! -L ${current_release}/checksums.txt ]] || \
    die "managed current checksums are missing or unsafe"
  [[ $(wc -l < "${current_release}/.artifact-sha256") -eq 1 ]] || die "managed current digest marker must contain one line"
  [[ $(wc -l < "${current_release}/.version") -eq 1 ]] || die "managed current version marker must contain one line"
  [[ $(stat -c '%a' -- "${current_release}/.artifact-sha256") == "444" ]] || \
    die "managed current digest marker mode is not normalized"
  [[ $(stat -c '%a' -- "${current_release}/.version") == "444" ]] || \
    die "managed current version marker mode is not normalized"
  marker_digest="$(< "${current_release}/.artifact-sha256")"
  marker_version="$(< "${current_release}/.version")"
  [[ ${marker_digest} =~ ^[0-9a-f]{64}$ ]] || die "managed current digest marker is invalid"
  [[ ${marker_version} =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$ ]] || \
    die "managed current version marker is invalid"
  expected_basename="${marker_version}-${marker_digest:0:12}"
  [[ $(basename -- "${current_release}") == "${expected_basename}" ]] || \
    die "managed current release name does not match its markers"
  [[ -f ${current_release}/bin/autostream-encoder-recorder &&
    ! -L ${current_release}/bin/autostream-encoder-recorder &&
    -x ${current_release}/bin/autostream-encoder-recorder ]] || \
    die "managed current canonical binary is missing or unsafe"
  [[ -z $(find "${current_release}" -type d ! -perm 0755 -print -quit) ]] || \
    die "managed current release directory modes are not normalized"
  [[ -z $(find "${current_release}" -type f \
    ! -path "${current_release}/.artifact-sha256" \
    ! -path "${current_release}/.version" \
    ! -perm 0644 ! -perm 0755 -print -quit) ]] || \
    die "managed current release file modes are not normalized"
  verify_release_checksum_inventory "${current_release}"
  (cd -- "${current_release}" && sha256sum --check --strict checksums.txt)
  current_version_output="$("${current_release}/bin/autostream-encoder-recorder" --version)"
  current_version_first_line="${current_version_output%%$'\n'*}"
  [[ ${current_version_first_line} == "autostream-encoder-recorder ${marker_version}" ]] || \
    die "managed current binary version does not match its release marker"
}

if [[ -e ${CURRENT_LINK} || -L ${CURRENT_LINK} ]]; then
  validate_existing_current
  previous_current_kind="symlink"
  previous_current_target="$(readlink -- "${CURRENT_LINK}")"
fi

existing_env_sha256=""
if [[ -e ${ENV_DEST} || -L ${ENV_DEST} ]]; then
  [[ -f ${ENV_DEST} && ! -L ${ENV_DEST} ]] || die "existing environment file is not a regular file"
  [[ $(stat -c '%U' -- "${ENV_DEST}") == "root" ]] || die "existing environment file must be owned by root"
  env_mode="$(stat -c '%a' -- "${ENV_DEST}")"
  [[ ${env_mode} == "600" || ${env_mode} == "640" ]] || \
    die "existing environment file must use mode 0600 or 0640"
  existing_env_sha256="$(sha256sum -- "${ENV_DEST}" | awk 'NR == 1 { print $1 }')"
else
  create_registered_temporary_path env_stage file \
    /etc/autostream/.encoder-recorder.env.install.XXXXXXXX || \
    die "could not create and journal environment staging"
  install -o root -g root -m 0640 "${RELEASE_DIR}/.env.example" "${env_stage}"
fi

[[ -d /etc/systemd/system && ! -L /etc/systemd/system ]] || die "systemd unit directory is missing or unsafe"
if [[ -e ${UNIT_DEST} || -L ${UNIT_DEST} ]]; then
  [[ -f ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || die "existing systemd unit is not a regular file"
  [[ $(stat -c '%U:%G' -- "${UNIT_DEST}") == "root:root" ]] || \
    die "existing systemd unit must be owned by root:root"
  unit_mode="$(stat -c '%a' -- "${UNIT_DEST}")"
  (( (8#${unit_mode} & 0022) == 0 )) || die "existing systemd unit must not be group/other writable"
  unit_previous_kind="regular"
  unit_previous_mode="${unit_mode}"
  unit_previous_sha256="$(sha256sum -- "${UNIT_DEST}" | awk 'NR == 1 { print $1 }')"
  copy_stable_source "${UNIT_DEST}" "${unit_previous_backup}"
  [[ $(sha256sum -- "${unit_previous_backup}" | awk 'NR == 1 { print $1 }') == "${unit_previous_sha256}" ]] || \
    die "staged previous systemd unit differs from its source"
fi
create_registered_temporary_path unit_stage file \
  /etc/systemd/system/.autostream-encoder-recorder.service.install.XXXXXXXX || \
  die "could not create and journal systemd unit staging"
install -o root -g root -m 0644 \
  "${RELEASE_DIR}/systemd/autostream-encoder-recorder.service.example" \
  "${unit_stage}"

ensure_private_root_directory "${LEGACY_BACKUP_DIR}"

preflight_public_path "${PUBLIC_BINARY}" "${CURRENT_LINK}/bin/autostream-encoder-recorder"
preflight_public_path "${PUBLIC_ALIAS}" "${PUBLIC_BINARY}"
if [[ ${previous_public_kind["${PUBLIC_BINARY}"]} == "absent" ]] &&
  systemctl is-active --quiet "${UNIT_NAME}"; then
  die "active service has no canonical public binary; stop it before installing"
fi
state_directory_mutation_started=true
ensure_directory "${STATE_DIR}" autostream autostream 0750
if [[ ${state_directory_previous_kind} == "absent" ]]; then
  state_directory_created_identity="$(stat -c '%d:%i' -- "${STATE_DIR}")"
fi
archive_directory_mutation_started=true
ensure_directory "${ARCHIVE_DIR}" autostream autostream 0750
if [[ ${archive_directory_previous_kind} == "absent" ]]; then
  archive_directory_created_identity="$(stat -c '%d:%i' -- "${ARCHIVE_DIR}")"
fi
activation_started=true
