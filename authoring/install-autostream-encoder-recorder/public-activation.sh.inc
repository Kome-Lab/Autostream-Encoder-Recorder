
backup_legacy_binary() {
  local link_path=$1
  local backup_name
  local backup_path
  local backup_stage
  local source_digest
  backup_name="$(basename -- "${link_path}")" || die "could not resolve the legacy public binary name"
  [[ -n ${backup_name} && ${backup_name} != */* ]] || die "legacy public binary name is unsafe"
  backup_path="${LEGACY_BACKUP_DIR}/${backup_name}"
  if [[ ! -e ${link_path} || -L ${link_path} ]]; then
    return
  fi
  ensure_private_root_directory "${LEGACY_BACKUP_DIR}"
  source_digest="${previous_public_sha256["${link_path}"]}"
  [[ $(sha256sum -- "${link_path}" | awk 'NR == 1 { print $1 }') == "${source_digest}" ]] || \
    die "legacy public binary changed after preflight: ${link_path}"
  case "${backup_previous_kind["${link_path}"]-unobserved}" in
    regular)
      [[ -f ${backup_path} && ! -L ${backup_path} &&
        $(stat -c '%d:%i' -- "${backup_path}") == "${backup_previous_identity["${link_path}"]}" &&
        $(stat -c '%u:%g:%a' -- "${backup_path}") == \
          "${backup_previous_uid["${link_path}"]}:${backup_previous_gid["${link_path}"]}:${backup_previous_mode["${link_path}"]}" &&
        $(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }') == \
          "${backup_previous_sha256["${link_path}"]}" ]] || \
        die "legacy public binary backup changed after preflight: ${link_path}"
      return
      ;;
    absent)
      [[ ! -e ${backup_path} && ! -L ${backup_path} ]] || \
        die "legacy public binary backup appeared after preflight: ${link_path}"
      ;;
    *)
      die "legacy public binary backup state was not captured: ${link_path}"
      ;;
  esac
  create_registered_temporary_path backup_stage file \
    "${LEGACY_BACKUP_DIR}/.${backup_name}.XXXXXXXX" || \
    die "could not create and journal legacy backup staging"
  install -o root -g root -m 0500 "${link_path}" "${backup_stage}"
  [[ $(sha256sum -- "${link_path}" | awk 'NR == 1 { print $1 }') == "${source_digest}" ]] || \
    die "legacy public binary changed while being backed up: ${link_path}"
  [[ $(sha256sum -- "${backup_stage}" | awk 'NR == 1 { print $1 }') == "${source_digest}" ]] || \
    die "legacy public binary backup differs from its source: ${link_path}"
  begin_installer_signal_transaction
  if ! mv -T -- "${backup_stage}" "${backup_path}"; then
    finish_installer_signal_transaction
    die "could not publish the legacy public binary backup: ${link_path}"
  fi
  forget_temporary_path "${backup_stage}"
  if ! backup_created_identity["${link_path}"]="$(stat -c '%d:%i' -- "${backup_path}")"; then
    rollback_incomplete=true
    finish_installer_signal_transaction
    die "could not journal the legacy public binary backup identity: ${link_path}"
  fi
  finish_installer_signal_transaction
  sync -f "${LEGACY_BACKUP_DIR}"
}

backup_legacy_binary "${PUBLIC_BINARY}"
backup_legacy_binary "${PUBLIC_ALIAS}"

prepare_public_rollback_anchor() {
  local link_path=$1
  local anchor_status=0
  local previous_identity="${previous_public_identity["${link_path}"]-}"
  local previous_nlink="${previous_public_nlink["${link_path}"]-}"
  local anchored_nlink=0
  local rollback_anchor="${link_path}.rollback-anchor.$$"

  [[ ${previous_public_kind["${link_path}"]} == "regular" ]] || return 0
  [[ ${previous_identity} =~ ^[0-9]+:[0-9]+$ &&
    ${previous_nlink} =~ ^[1-9][0-9]*$ &&
    -f ${link_path} && ! -L ${link_path} &&
    $(stat -c '%d:%i' -- "${link_path}") == "${previous_identity}" &&
    $(stat -c '%h' -- "${link_path}") == "${previous_nlink}" &&
    -z ${public_rollback_anchor_path["${link_path}"]+present} &&
    -z ${public_rollback_anchor_owner["${rollback_anchor}"]+present} &&
    ! -e ${rollback_anchor} && ! -L ${rollback_anchor} ]] || return 1

  begin_installer_signal_transaction
  if ! register_temporary_path "${rollback_anchor}"; then
    anchor_status=1
  else
    public_rollback_anchor_path["${link_path}"]="${rollback_anchor}"
    public_rollback_anchor_owner["${rollback_anchor}"]="${link_path}"
    if ! ln -T -- "${link_path}" "${rollback_anchor}"; then
      anchor_status=1
    elif ! claim_temporary_path "${rollback_anchor}"; then
      anchor_status=1
    else
      anchored_nlink=$((previous_nlink + 1))
    fi
    if [[ ${anchor_status} -eq 0 &&
      ( ${temporary_path_identity["${rollback_anchor}"]} != "${previous_identity}" ||
      $(stat -c '%d:%i' -- "${link_path}") != "${previous_identity}" ||
      $(stat -c '%d:%i' -- "${rollback_anchor}") != "${previous_identity}" ||
      $(stat -c '%h' -- "${link_path}") != "${anchored_nlink}" ||
      $(stat -c '%h' -- "${rollback_anchor}") != "${anchored_nlink}" ) ]]; then
      anchor_status=1
    fi
    if [[ ${anchor_status} -ne 0 &&
      ! -e ${rollback_anchor} && ! -L ${rollback_anchor} ]]; then
      forget_temporary_path "${rollback_anchor}"
      unset 'public_rollback_anchor_owner['"${rollback_anchor}"']'
      unset 'public_rollback_anchor_path['"${link_path}"']'
    fi
  fi
  finish_installer_signal_transaction
  [[ ${anchor_status} -eq 0 ]]
}

install_public_link() {
  local target=$1
  local link_path=$2
  local link_status=0
  local previous_kind="${previous_public_kind["${link_path}"]}"
  local previous_identity="${previous_public_identity["${link_path}"]-}"
  local previous_nlink="${previous_public_nlink["${link_path}"]-}"
  local rollback_anchor="${public_rollback_anchor_path["${link_path}"]-}"
  local staged_link_identity=""
  public_link_next="${link_path}.next.$$"
  [[ ! -e ${public_link_next} && ! -L ${public_link_next} ]] || \
    die "public link staging path already exists: ${public_link_next}"
  begin_installer_signal_transaction
  if ! register_temporary_path "${public_link_next}"; then
    link_status=1
  elif ! ln -s -- "${target}" "${public_link_next}"; then
    link_status=1
  elif ! claim_temporary_path "${public_link_next}"; then
    link_status=1
  else
    staged_link_identity="${temporary_path_identity["${public_link_next}"]}"
    [[ ${staged_link_identity} =~ ^[0-9]+:[0-9]+$ ]] || link_status=1
    if [[ ${link_status} -eq 0 && ${previous_kind} == "regular" ]]; then
      [[ ${previous_identity} =~ ^[0-9]+:[0-9]+$ &&
        ${previous_nlink} =~ ^[1-9][0-9]*$ &&
        -n ${rollback_anchor} &&
        ${public_rollback_anchor_owner["${rollback_anchor}"]-} == "${link_path}" &&
        -n ${temporary_path_identity["${rollback_anchor}"]+present} &&
        ${temporary_path_identity["${rollback_anchor}"]} == "${previous_identity}" &&
        -f ${rollback_anchor} && ! -L ${rollback_anchor} &&
        $(stat -c '%d:%i' -- "${rollback_anchor}") == "${previous_identity}" &&
        $(stat -c '%h' -- "${rollback_anchor}") == "$((previous_nlink + 1))" &&
        -f ${link_path} && ! -L ${link_path} &&
        $(stat -c '%d:%i' -- "${link_path}") == "${previous_identity}" &&
        $(stat -c '%h' -- "${link_path}") == "$((previous_nlink + 1))" ]] || link_status=1
    fi
    if [[ ${link_status} -eq 0 ]]; then
      published_public_target["${link_path}"]="${target}"
      published_public_identity["${link_path}"]="${staged_link_identity}"
      if ! mv -Tf -- "${public_link_next}" "${link_path}"; then
        link_status=1
      else
        forget_temporary_path "${public_link_next}"
        public_link_next=""
        [[ -L ${link_path} &&
          $(stat -c '%d:%i' -- "${link_path}") == "${staged_link_identity}" &&
          $(readlink -- "${link_path}") == "${target}" ]] || link_status=1
      fi
    fi
  fi
  finish_installer_signal_transaction
  [[ ${link_status} -eq 0 ]] || die "could not publish public link: ${link_path}"
  sync -f "$(dirname -- "${link_path}")"
}

if [[ -n ${env_stage} ]]; then
  begin_installer_signal_transaction
  if ! mv -Tf -- "${env_stage}" "${ENV_DEST}"; then
    finish_installer_signal_transaction
    die "could not publish the environment file"
  fi
  forget_temporary_path "${env_stage}"
  env_stage=""
  env_created=true
  if ! env_created_identity="$(stat -c '%d:%i' -- "${ENV_DEST}")"; then
    rollback_incomplete=true
    finish_installer_signal_transaction
    die "could not journal the published environment file identity"
  fi
  finish_installer_signal_transaction
  sync -f "$(dirname -- "${ENV_DEST}")"
fi
if [[ -n ${existing_env_sha256} ]]; then
  [[ $(sha256sum -- "${ENV_DEST}" | awk 'NR == 1 { print $1 }') == "${existing_env_sha256}" ]] || \
    die "existing environment file changed during installation"
fi

begin_installer_signal_transaction
if ! mv -Tf -- "${unit_stage}" "${UNIT_DEST}"; then
  finish_installer_signal_transaction
  die "could not publish the systemd unit"
fi
forget_temporary_path "${unit_stage}"
unit_stage=""
unit_replaced=true
finish_installer_signal_transaction
sync -f /etc/systemd/system
[[ $(stat -c '%U:%G:%a' -- "${UNIT_DEST}") == "root:root:644" ]] || \
  die "installed systemd unit failed ownership or mode verification"
systemctl daemon-reload

# Prepare current while a legacy active unit still resolves its unchanged
# regular public binary. Public links are the final activation mutation.
if [[ ! -L ${CURRENT_LINK} ]] || [[ $(readlink -f -- "${CURRENT_LINK}") != "${RELEASE_DIR}" ]]; then
  current_publish_status=0
  current_next="${MANAGED_ROOT}/.current.next.$$"
  [[ ! -e ${current_next} && ! -L ${current_next} ]] || die "managed current staging path already exists"
  begin_installer_signal_transaction
  if ! register_temporary_path "${current_next}"; then
    current_publish_status=1
  elif ! ln -s -- "${RELEASE_DIR}" "${current_next}"; then
    current_publish_status=1
  elif ! claim_temporary_path "${current_next}"; then
    current_publish_status=1
  elif ! mv -Tf -- "${current_next}" "${CURRENT_LINK}"; then
    current_publish_status=1
  else
    forget_temporary_path "${current_next}"
    current_next=""
  fi
  finish_installer_signal_transaction
  [[ ${current_publish_status} -eq 0 ]] || die "could not publish the managed current link"
  sync -f "${MANAGED_ROOT}"
fi
[[ $(readlink -f -- "${CURRENT_LINK}") == "${RELEASE_DIR}" ]] || die "managed current link verification failed"

prepare_public_rollback_anchor "${PUBLIC_BINARY}" || \
  die "could not prepare the canonical public binary rollback anchor"
prepare_public_rollback_anchor "${PUBLIC_ALIAS}" || \
  die "could not prepare the public alias rollback anchor"
if [[ -n ${public_rollback_anchor_path["${PUBLIC_BINARY}"]-} ||
  -n ${public_rollback_anchor_path["${PUBLIC_ALIAS}"]-} ]]; then
  sync -f /usr/local/bin
fi

# The alias is not used by ExecStart, so switch it first and commit the
# canonical systemd path last.
install_public_link "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}"
[[ $(readlink -- "${PUBLIC_ALIAS}") == "${PUBLIC_BINARY}" ]] || \
  die "public compatibility symlink target is incorrect"
install_public_link "${CURRENT_LINK}/bin/autostream-encoder-recorder" "${PUBLIC_BINARY}"
[[ $(readlink -- "${PUBLIC_BINARY}") == "${CURRENT_LINK}/bin/autostream-encoder-recorder" ]] || \
  die "public canonical binary symlink target is incorrect"
[[ $(readlink -f -- "${PUBLIC_BINARY}") == "${RELEASE_DIR}/bin/autostream-encoder-recorder" ]] || \
  die "public canonical binary link verification failed"
[[ $(readlink -f -- "${PUBLIC_ALIAS}") == "${RELEASE_DIR}/bin/autostream-encoder-recorder" ]] || \
  die "public compatibility binary link verification failed"
installation_complete=true

printf 'Installed AutoStream Encoder Recorder %s.\n' "${VERSION}"
printf 'Existing settings were preserved. Review %s and run the Panel-generated configure command.\n' "${ENV_DEST}"
if systemctl is-active --quiet "${UNIT_NAME}"; then
  printf 'The service is already active; load the installed release with:\n'
  printf '  sudo systemctl restart %s\n' "${UNIT_NAME}"
else
  printf 'Start this service for the first time with:\n'
  printf '  sudo systemctl enable --now %s\n' "${UNIT_NAME}"
fi
printf '  sudo systemctl status %s\n' "${UNIT_NAME}"
