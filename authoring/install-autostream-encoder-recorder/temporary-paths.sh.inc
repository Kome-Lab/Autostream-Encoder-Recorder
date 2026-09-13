
register_temporary_path() {
  local path=$1
  [[ -z ${temporary_path_identity["${path}"]+present} ]] || return 1
  temporary_path_identity["${path}"]="pending"
}

claim_temporary_path() {
  local path=$1
  [[ -n ${temporary_path_identity["${path}"]+present} &&
    ${temporary_path_identity["${path}"]} == "pending" &&
    ( -e ${path} || -L ${path} ) ]] || return 1
  temporary_path_identity["${path}"]="$(stat -c '%d:%i' -- "${path}")"
}

create_registered_temporary_path() {
  local output_variable=$1
  local path_kind=$2
  local template=$3
  local path=""

  # mktemp and journal claim form one signal-safe ownership transaction. A
  # termination request is delivered only after the new inode is journaled.
  begin_installer_signal_transaction
  if [[ ${path_kind} == "directory" ]]; then
    if ! path="$(mktemp -d "${template}")"; then
      finish_installer_signal_transaction
      return 1
    fi
  elif [[ ${path_kind} == "file" ]]; then
    if ! path="$(mktemp "${template}")"; then
      finish_installer_signal_transaction
      return 1
    fi
  else
    finish_installer_signal_transaction
    return 1
  fi
  if ! register_temporary_path "${path}" ||
    ! claim_temporary_path "${path}"; then
    if [[ ${path_kind} == "directory" ]]; then
      rm -rf -- "${path}"
    else
      rm -f -- "${path}"
    fi
    forget_temporary_path "${path}"
    finish_installer_signal_transaction
    return 1
  fi
  printf -v "${output_variable}" '%s' "${path}"
  finish_installer_signal_transaction
}

forget_temporary_path() {
  local path=$1
  unset 'temporary_path_identity['"${path}"']'
}

finalize_public_rollback_anchors() {
  local link_path
  local rollback_anchor
  local previous_identity
  local previous_nlink
  local rollback_anchor_parent
  local cleanup_failed=false

  for link_path in "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}"; do
    [[ ${previous_public_kind["${link_path}"]} == "regular" ]] || continue
    rollback_anchor="${public_rollback_anchor_path["${link_path}"]-}"
    previous_identity="${previous_public_identity["${link_path}"]-}"
    previous_nlink="${previous_public_nlink["${link_path}"]-}"
    if [[ -z ${rollback_anchor} ||
      ! ${previous_identity} =~ ^[0-9]+:[0-9]+$ ||
      ! ${previous_nlink} =~ ^[1-9][0-9]*$ ||
      ${public_rollback_anchor_owner["${rollback_anchor}"]-} != "${link_path}" ||
      -z ${temporary_path_identity["${rollback_anchor}"]+present} ||
      ${temporary_path_identity["${rollback_anchor}"]} != "${previous_identity}" ||
      ! -f ${rollback_anchor} || -L ${rollback_anchor} ||
      $(stat -c '%d:%i' -- "${rollback_anchor}") != "${previous_identity}" ||
      $(stat -c '%h' -- "${rollback_anchor}") != "${previous_nlink}" ||
      -z ${published_public_identity["${link_path}"]-} ||
      -z ${published_public_target["${link_path}"]-} ||
      ! -L ${link_path} ||
      $(stat -c '%d:%i' -- "${link_path}") != "${published_public_identity["${link_path}"]}" ||
      $(readlink -- "${link_path}") != "${published_public_target["${link_path}"]}" ]]; then
      cleanup_failed=true
      continue
    fi
    if ! rm -f -- "${rollback_anchor}" ||
      [[ -e ${rollback_anchor} || -L ${rollback_anchor} ]]; then
      cleanup_failed=true
      continue
    fi
    rollback_anchor_parent="$(dirname -- "${rollback_anchor}")"
    if ! sync -f "${rollback_anchor_parent}"; then
      cleanup_failed=true
      continue
    fi
    forget_temporary_path "${rollback_anchor}"
    unset 'public_rollback_anchor_owner['"${rollback_anchor}"']'
    unset 'public_rollback_anchor_path['"${link_path}"']'
  done
  [[ ${cleanup_failed} == false ]]
}

cleanup_temporary_paths() {
  local path
  local identity
  local cleanup_failed=false
  for path in "${!temporary_path_identity[@]}"; do
    identity="${temporary_path_identity["${path}"]}"
    if [[ -n ${public_rollback_anchor_owner["${path}"]-} ]]; then
      cleanup_failed=true
      continue
    fi
    if [[ ! -e ${path} && ! -L ${path} ]]; then
      unset 'temporary_path_identity['"${path}"']'
      continue
    fi
    if [[ ${identity} == "pending" ||
      $(stat -c '%d:%i' -- "${path}") != "${identity}" ]]; then
      cleanup_failed=true
      continue
    fi
    rm -f -- "${path}" || cleanup_failed=true
    if [[ -e ${path} || -L ${path} ]]; then
      cleanup_failed=true
    else
      unset 'temporary_path_identity['"${path}"']'
    fi
  done
  [[ ${cleanup_failed} == false ]]
}
