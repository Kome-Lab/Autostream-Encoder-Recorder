
restore_existing_state_directory() {
  local state_parent
  local created_identity="${state_directory_created_identity}"
  [[ ${state_directory_snapshot_done} == true ]] || return 1
  state_parent="$(dirname -- "${STATE_DIR}")" || return 1
  if [[ -z ${created_identity} ]]; then
    created_identity="${journaled_directory_created_identity["${STATE_DIR}"]-}"
  fi
  case "${state_directory_previous_kind}" in
    directory)
      [[ -d ${STATE_DIR} && ! -L ${STATE_DIR} ]] || return 1
      [[ $(stat -c '%d:%i' -- "${STATE_DIR}") == "${state_directory_previous_identity}" ]] || return 1
      chown "${state_directory_previous_uid}:${state_directory_previous_gid}" "${STATE_DIR}" || return 1
      chmod "${state_directory_previous_mode}" "${STATE_DIR}" || return 1
      [[ $(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}") == \
        "${state_directory_previous_identity}:${state_directory_previous_uid}:${state_directory_previous_gid}:${state_directory_previous_mode}" ]] || return 1
      sync -f "${STATE_DIR}" || return 1
      ;;
    absent)
      if [[ -e ${STATE_DIR} || -L ${STATE_DIR} ]]; then
        [[ -d ${STATE_DIR} && ! -L ${STATE_DIR} &&
          -n ${created_identity} &&
          $(stat -c '%d:%i' -- "${STATE_DIR}") == "${created_identity}" ]] || return 1
        [[ -z $(find "${STATE_DIR}" -mindepth 1 -print -quit) ]] || return 1
        rmdir -- "${STATE_DIR}" || return 1
      fi
      [[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || return 1
      sync -f "${state_parent}" || return 1
      ;;
    *)
      return 1
      ;;
  esac
}

restore_existing_archive_directory() {
  local archive_parent
  local created_identity="${archive_directory_created_identity}"
  [[ ${archive_directory_snapshot_done} == true ]] || return 1
  archive_parent="$(dirname -- "${ARCHIVE_DIR}")" || return 1
  if [[ -z ${created_identity} ]]; then
    created_identity="${journaled_directory_created_identity["${ARCHIVE_DIR}"]-}"
  fi
  case "${archive_directory_previous_kind}" in
    directory)
      [[ -d ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} ]] || return 1
      [[ $(stat -c '%d:%i' -- "${ARCHIVE_DIR}") == "${archive_directory_previous_identity}" ]] || return 1
      chown "${archive_directory_previous_uid}:${archive_directory_previous_gid}" "${ARCHIVE_DIR}" || return 1
      chmod "${archive_directory_previous_mode}" "${ARCHIVE_DIR}" || return 1
      [[ $(stat -c '%d:%i:%u:%g:%a' -- "${ARCHIVE_DIR}") == \
        "${archive_directory_previous_identity}:${archive_directory_previous_uid}:${archive_directory_previous_gid}:${archive_directory_previous_mode}" ]] || return 1
      sync -f "${ARCHIVE_DIR}" || return 1
      ;;
    absent)
      if [[ -e ${ARCHIVE_DIR} || -L ${ARCHIVE_DIR} ]]; then
        [[ -d ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} &&
          -n ${created_identity} &&
          $(stat -c '%d:%i' -- "${ARCHIVE_DIR}") == "${created_identity}" ]] || return 1
        [[ -z $(find "${ARCHIVE_DIR}" -mindepth 1 -print -quit) ]] || return 1
        rmdir -- "${ARCHIVE_DIR}" || return 1
      fi
      [[ ! -e ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} ]] || return 1
      sync -f "${archive_parent}" || return 1
      ;;
    *)
      return 1
      ;;
  esac
}

restore_current_state() {
  local restore_next="${MANAGED_ROOT}/.current.rollback.$$"
  [[ ! -e ${restore_next} && ! -L ${restore_next} ]] || return 1
  case "${previous_current_kind}" in
    absent)
      if [[ -e ${CURRENT_LINK} && ! -L ${CURRENT_LINK} ]]; then
        return 1
      fi
      rm -f -- "${CURRENT_LINK}" || return 1
      [[ ! -e ${CURRENT_LINK} && ! -L ${CURRENT_LINK} ]] || return 1
      ;;
    symlink)
      if [[ -e ${CURRENT_LINK} && ! -L ${CURRENT_LINK} ]]; then
        return 1
      fi
      register_temporary_path "${restore_next}" || return 1
      ln -s -- "${previous_current_target}" "${restore_next}" || return 1
      claim_temporary_path "${restore_next}" || return 1
      mv -Tf -- "${restore_next}" "${CURRENT_LINK}" || return 1
      forget_temporary_path "${restore_next}"
      [[ -L ${CURRENT_LINK} &&
        $(readlink -- "${CURRENT_LINK}") == "${previous_current_target}" ]] || return 1
      ;;
    *)
      return 1
      ;;
  esac
  sync -f "${MANAGED_ROOT}" || return 1
}

restore_legacy_backup_state() {
  local path=$1
  local backup_name
  local backup_path
  backup_name="$(basename -- "${path}")" || return 1
  backup_path="${LEGACY_BACKUP_DIR}/${backup_name}"
  case "${backup_previous_kind["${path}"]-not-applicable}" in
    not-applicable)
      return 0
      ;;
    regular)
      [[ -f ${backup_path} && ! -L ${backup_path} &&
        $(stat -c '%d:%i' -- "${backup_path}") == "${backup_previous_identity["${path}"]}" &&
        $(stat -c '%u:%g:%a' -- "${backup_path}") == \
          "${backup_previous_uid["${path}"]}:${backup_previous_gid["${path}"]}:${backup_previous_mode["${path}"]}" &&
        $(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }') == \
          "${backup_previous_sha256["${path}"]}" ]] || return 1
      ;;
    absent)
      if [[ -z ${backup_created_identity["${path}"]-} ]]; then
        [[ ! -e ${backup_path} && ! -L ${backup_path} ]] || return 1
        return 0
      fi
      [[ -f ${backup_path} && ! -L ${backup_path} &&
        $(stat -c '%d:%i' -- "${backup_path}") == "${backup_created_identity["${path}"]}" ]] || return 1
      rm -f -- "${backup_path}" || return 1
      [[ ! -e ${backup_path} && ! -L ${backup_path} ]] || return 1
      sync -f "${LEGACY_BACKUP_DIR}" || return 1
      backup_created_identity["${path}"]=""
      ;;
    *)
      return 1
      ;;
  esac
}

restore_public_state() {
  local path=$1
  local kind="${previous_public_kind["${path}"]}"
  local restore_next="${path}.rollback.$$"
  local previous_identity="${previous_public_identity["${path}"]-}"
  local previous_nlink="${previous_public_nlink["${path}"]-}"
  local published_identity="${published_public_identity["${path}"]-}"
  local published_target="${published_public_target["${path}"]-}"
  local rollback_anchor="${public_rollback_anchor_path["${path}"]-}"
  local rollback_anchor_consumed=false
  [[ ! -e ${restore_next} && ! -L ${restore_next} ]] || return 1
  if [[ -e ${path} && ! -L ${path} && ! -f ${path} ]]; then
    return 1
  fi
  case "${kind}" in
    absent)
      if [[ -e ${path} || -L ${path} ]]; then
        [[ ${published_identity} =~ ^[0-9]+:[0-9]+$ &&
          -n ${published_target} && -L ${path} &&
          $(stat -c '%d:%i' -- "${path}") == "${published_identity}" &&
          $(readlink -- "${path}") == "${published_target}" ]] || return 1
        rm -f -- "${path}" || return 1
      fi
      [[ ! -e ${path} && ! -L ${path} ]] || return 1
      ;;
    symlink)
      if [[ -L ${path} && ${previous_identity} =~ ^[0-9]+:[0-9]+$ &&
        $(stat -c '%d:%i' -- "${path}") == "${previous_identity}" &&
        $(readlink -- "${path}") == "${previous_public_target["${path}"]}" ]]; then
        :
      else
        if [[ -e ${path} || -L ${path} ]]; then
          [[ ${published_identity} =~ ^[0-9]+:[0-9]+$ &&
            -n ${published_target} && -L ${path} &&
            $(stat -c '%d:%i' -- "${path}") == "${published_identity}" &&
            $(readlink -- "${path}") == "${published_target}" ]] || return 1
        fi
        register_temporary_path "${restore_next}" || return 1
        ln -s -- "${previous_public_target["${path}"]}" "${restore_next}" || return 1
        claim_temporary_path "${restore_next}" || return 1
        mv -Tf -- "${restore_next}" "${path}" || return 1
        forget_temporary_path "${restore_next}"
      fi
      [[ -L ${path} &&
        $(readlink -- "${path}") == "${previous_public_target["${path}"]}" ]] || return 1
      ;;
    regular)
      [[ ${previous_identity} =~ ^[0-9]+:[0-9]+$ &&
        ${previous_nlink} =~ ^[1-9][0-9]*$ ]] || return 1
      if [[ -f ${path} && ! -L ${path} &&
        $(stat -c '%d:%i' -- "${path}") == "${previous_identity}" &&
        $(sha256sum -- "${path}" | awk 'NR == 1 { print $1 }') == "${previous_public_sha256["${path}"]}" ]]; then
        chmod "${previous_public_mode["${path}"]}" "${path}" || return 1
        if [[ -n ${rollback_anchor} ]]; then
          [[ ${public_rollback_anchor_owner["${rollback_anchor}"]-} == "${path}" &&
            -n ${temporary_path_identity["${rollback_anchor}"]+present} &&
            ${temporary_path_identity["${rollback_anchor}"]} == "${previous_identity}" &&
            -f ${rollback_anchor} && ! -L ${rollback_anchor} &&
            $(stat -c '%d:%i' -- "${rollback_anchor}") == "${previous_identity}" &&
            $(stat -c '%h' -- "${rollback_anchor}") == "$((previous_nlink + 1))" ]] || return 1
          rm -f -- "${rollback_anchor}" || return 1
          [[ ! -e ${rollback_anchor} && ! -L ${rollback_anchor} ]] || return 1
          rollback_anchor_consumed=true
        fi
      else
        [[ -n ${rollback_anchor} && -n ${published_target} &&
          ${published_identity} =~ ^[0-9]+:[0-9]+$ &&
          ${public_rollback_anchor_owner["${rollback_anchor}"]-} == "${path}" &&
          -n ${temporary_path_identity["${rollback_anchor}"]+present} &&
          ${temporary_path_identity["${rollback_anchor}"]} == "${previous_identity}" &&
          -f ${rollback_anchor} && ! -L ${rollback_anchor} &&
          $(stat -c '%d:%i' -- "${rollback_anchor}") == "${previous_identity}" &&
          $(stat -c '%h' -- "${rollback_anchor}") == "${previous_nlink}" &&
          -L ${path} &&
          $(stat -c '%d:%i' -- "${path}") == "${published_identity}" &&
          $(readlink -- "${path}") == "${published_target}" ]] || return 1
        mv -Tf -- "${rollback_anchor}" "${path}" || return 1
        rollback_anchor_consumed=true
      fi
      [[ -f ${path} && ! -L ${path} &&
        $(stat -c '%d:%i' -- "${path}") == "${previous_public_identity["${path}"]}" &&
        $(stat -c '%h' -- "${path}") == "${previous_public_nlink["${path}"]}" &&
        $(stat -c '%U:%G:%a' -- "${path}") == "root:root:${previous_public_mode["${path}"]}" &&
        $(sha256sum -- "${path}" | awk 'NR == 1 { print $1 }') == \
          "${previous_public_sha256["${path}"]}" ]] || return 1
      sync -f "$(dirname -- "${path}")" || return 1
      if [[ ${rollback_anchor_consumed} == true ]]; then
        forget_temporary_path "${rollback_anchor}"
        unset 'public_rollback_anchor_owner['"${rollback_anchor}"']'
        unset 'public_rollback_anchor_path['"${path}"']'
      fi
      unset 'published_public_target['"${path}"']'
      unset 'published_public_identity['"${path}"']'
      # A backup is removed only after the public binary is proven restored.
      # On incomplete rollback it remains as a durable recovery backup.
      restore_legacy_backup_state "${path}" || return 1
      ;;
    *)
      return 1
      ;;
  esac
  sync -f "$(dirname -- "${path}")" || return 1
}

restore_unit_state() {
  local restore_next=""
  [[ ${unit_replaced} == true ]] || return 0
  case "${unit_previous_kind}" in
    absent)
      [[ ! -L ${UNIT_DEST} ]] || return 1
      [[ ! -e ${UNIT_DEST} || -f ${UNIT_DEST} ]] || return 1
      rm -f -- "${UNIT_DEST}" || return 1
      [[ ! -e ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || return 1
      ;;
    regular)
      [[ -f ${unit_previous_backup} && ! -L ${unit_previous_backup} ]] || return 1
      create_registered_temporary_path restore_next file \
        /etc/systemd/system/.autostream-encoder-recorder.service.rollback.XXXXXXXX || return 1
      install -o root -g root -m "${unit_previous_mode}" \
        "${unit_previous_backup}" "${restore_next}" || return 1
      [[ $(sha256sum -- "${restore_next}" | awk 'NR == 1 { print $1 }') == \
        "${unit_previous_sha256}" ]] || return 1
      mv -Tf -- "${restore_next}" "${UNIT_DEST}" || return 1
      forget_temporary_path "${restore_next}"
      [[ -f ${UNIT_DEST} && ! -L ${UNIT_DEST} &&
        $(stat -c '%U:%G:%a' -- "${UNIT_DEST}") == "root:root:${unit_previous_mode}" &&
        $(sha256sum -- "${UNIT_DEST}" | awk 'NR == 1 { print $1 }') == \
          "${unit_previous_sha256}" ]] || return 1
      ;;
    *)
      return 1
      ;;
  esac
  sync -f /etc/systemd/system || return 1
}
