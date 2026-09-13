
journal_directory_before_mutation() {
  local path=$1
  local snapshot_status=0
  local snapshot_identity=""
  local snapshot_uid=""
  local snapshot_gid=""
  local snapshot_mode=""
  [[ -z ${journaled_directory_recorded["${path}"]+present} ]] || return 0
  # A pending signal is delivered only after the rollback journal is complete.
  begin_installer_signal_transaction
  if [[ -e ${path} || -L ${path} ]]; then
    if [[ ! -d ${path} || -L ${path} ]]; then
      snapshot_status=1
    elif ! snapshot_identity="$(stat -c '%d:%i' -- "${path}")" ||
      ! snapshot_uid="$(stat -c '%u' -- "${path}")" ||
      ! snapshot_gid="$(stat -c '%g' -- "${path}")" ||
      ! snapshot_mode="$(stat -c '%a' -- "${path}")"; then
      snapshot_status=1
    fi
  fi
  if [[ ${snapshot_status} -eq 0 ]]; then
    journaled_directory_recorded["${path}"]=true
    journaled_directory_order+=("${path}")
    if [[ -e ${path} || -L ${path} ]]; then
      journaled_directory_previous_kind["${path}"]="directory"
      journaled_directory_previous_identity["${path}"]="${snapshot_identity}"
      journaled_directory_previous_uid["${path}"]="${snapshot_uid}"
      journaled_directory_previous_gid["${path}"]="${snapshot_gid}"
      journaled_directory_previous_mode["${path}"]="${snapshot_mode}"
    else
      journaled_directory_previous_kind["${path}"]="absent"
    fi
  fi
  finish_installer_signal_transaction
  [[ ${snapshot_status} -eq 0 ]]
}

install_journaled_directory() {
  local path=$1
  local owner=$2
  local group=$3
  local mode=$4
  local install_status=0
  local created_identity=""

  # Directory creation/normalization and identity capture are one journaled
  # mutation. A pending signal is delivered after rollback ownership is
  # complete.
  begin_installer_signal_transaction
  if install -d -o "${owner}" -g "${group}" -m "${mode}" "${path}"; then
    :
  else
    install_status=$?
  fi
  if [[ ${journaled_directory_previous_kind["${path}"]} == "absent" ]]; then
    if [[ -d ${path} && ! -L ${path} ]] &&
      created_identity="$(stat -c '%d:%i' -- "${path}")"; then
      journaled_directory_created_identity["${path}"]="${created_identity}"
    elif [[ ${install_status} -eq 0 ]]; then
      install_status=1
    fi
  fi
  finish_installer_signal_transaction
  [[ ${install_status} -eq 0 ]]
}

rollback_journaled_directories() {
  local index
  local path
  local parent
  local rollback_failed=false
  for ((index=${#journaled_directory_order[@]} - 1; index >= 0; index--)); do
    path="${journaled_directory_order[index]}"
    case "${journaled_directory_previous_kind["${path}"]}" in
      directory)
        if [[ ! -d ${path} || -L ${path} ||
          $(stat -c '%d:%i' -- "${path}") != "${journaled_directory_previous_identity["${path}"]}" ]]; then
          rollback_failed=true
          continue
        fi
        chown "${journaled_directory_previous_uid["${path}"]}:${journaled_directory_previous_gid["${path}"]}" \
          "${path}" || rollback_failed=true
        chmod "${journaled_directory_previous_mode["${path}"]}" "${path}" || rollback_failed=true
        [[ $(stat -c '%d:%i:%u:%g:%a' -- "${path}") == \
          "${journaled_directory_previous_identity["${path}"]}:${journaled_directory_previous_uid["${path}"]}:${journaled_directory_previous_gid["${path}"]}:${journaled_directory_previous_mode["${path}"]}" ]] || \
          rollback_failed=true
        ;;
      absent)
        if [[ ! -e ${path} && ! -L ${path} ]]; then
          continue
        fi
        if [[ ! -d ${path} || -L ${path} ||
          -z ${journaled_directory_created_identity["${path}"]-} ||
          $(stat -c '%d:%i' -- "${path}") != "${journaled_directory_created_identity["${path}"]}" ||
          -n $(find "${path}" -mindepth 1 -print -quit) ]]; then
          rollback_failed=true
          continue
        fi
        rmdir -- "${path}" || rollback_failed=true
        parent="$(dirname -- "${path}")"
        sync -f "${parent}" || rollback_failed=true
        ;;
      *)
        rollback_failed=true
        ;;
    esac
  done
  [[ ${rollback_failed} == false ]]
}

rollback_created_release() {
  [[ ${release_created} == true ]] || return 0
  [[ -d ${RELEASE_DIR} && ! -L ${RELEASE_DIR} &&
    $(stat -c '%d:%i' -- "${RELEASE_DIR}") == "${release_created_identity}" &&
    $(dirname -- "${RELEASE_DIR}") == "${RELEASES_DIR}" ]] || return 1
  rm -rf -- "${RELEASE_DIR}" || return 1
  [[ ! -e ${RELEASE_DIR} && ! -L ${RELEASE_DIR} ]] || return 1
  sync -f "${RELEASES_DIR}" || return 1
  release_created=false
  release_created_identity=""
}
