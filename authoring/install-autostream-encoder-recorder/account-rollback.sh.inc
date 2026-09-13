
local_group_database_references_are_safe() {
  local account_name

  [[ -f /etc/group && ! -L /etc/group && -r /etc/group &&
    -f /etc/gshadow && ! -L /etc/gshadow && -r /etc/gshadow ]] || return 1
  for account_name in autostream "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"; do
    awk -F: -v target="${account_name}" '
      function list_contains(value, count, member_index, names) {
        count = split(value, names, ",")
        for (member_index = 1; member_index <= count; member_index++) {
          if (names[member_index] == target) {
            return 1
          }
        }
        return 0
      }
      FILENAME == "/etc/group" && NF >= 4 && list_contains($4) {
        unsafe = 1
      }
      FILENAME == "/etc/gshadow" && NF >= 4 &&
          (list_contains($3) || list_contains($4)) {
        unsafe = 1
      }
      END {
        exit unsafe ? 1 : 0
      }
    ' /etc/group /etc/gshadow || return 1
  done
}

calculate_local_group_database_digest() {
  local digest
  local path=$1

  case "${path}" in
    /etc/group|/etc/gshadow)
      ;;
    *)
      return 1
      ;;
  esac
  digest="$(sha256sum -- "${path}" | awk 'NR == 1 { print $1 }')" || return 1
  [[ ${digest} =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s\n' "${digest}"
}

local_group_database_digests_match() {
  local current_group_digest
  local current_gshadow_digest
  local expected_group_digest=$1
  local expected_gshadow_digest=$2

  current_group_digest="$(calculate_local_group_database_digest /etc/group)" || return 1
  current_gshadow_digest="$(calculate_local_group_database_digest /etc/gshadow)" || return 1
  [[ ${current_group_digest} == "${expected_group_digest}" &&
    ${current_gshadow_digest} == "${expected_gshadow_digest}" ]]
}

prepare_autostream_user_rollback_login() {
  if id autostream >/dev/null 2>&1; then
    return 0
  fi
  [[ -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  autostream_user_rollback_login_ready=true
}

restore_renamed_autostream_user() {
  local protected_group_record=$1
  local renamed_user_record=$2
  local group_content_digest=$3
  local gshadow_content_digest=$4
  local restoration_failed=false

  [[ -z $(getent passwd autostream || true) ]] || return 1
  [[ "$(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" || true)" == \
    "${renamed_user_record}" ]] || return 1
  if ! usermod --login autostream "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"; then
    restoration_failed=true
  fi
  [[ "$(getent passwd autostream || true)" == "${created_autostream_user_record}" ]] || return 1
  [[ -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" || true) ]] || return 1
  [[ "$(getent group autostream || true)" == "${protected_group_record}" ]] || return 1
  [[ -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" || true) ]] || return 1
  local_group_database_digests_match \
    "${group_content_digest}" "${gshadow_content_digest}" || return 1
  [[ ${restoration_failed} == false ]]
}

remove_created_autostream_user_preserving_group() {
  local delete_status=0
  local expected_group_gid
  local group_content_digest
  local gshadow_content_digest
  local rename_status=0
  local renamed_user_record
  local user_record_suffix

  [[ ${created_autostream_user} == true &&
    ${autostream_user_rollback_login_ready} == true &&
    ${created_autostream_user_record} =~ ^autostream(:[^:]*:[1-9][0-9]*:[1-9][0-9]*:[^:]*:/var/lib/autostream:/usr/sbin/nologin)$ ]] || \
    return 1
  user_record_suffix="${BASH_REMATCH[1]}"
  [[ $(getent passwd autostream 2>/dev/null || true) == \
      "${created_autostream_user_record}" &&
    -n ${created_autostream_user_group_record} &&
    $(getent group autostream 2>/dev/null || true) == \
      "${created_autostream_user_group_record}" ]] || return 1
  expected_group_gid="$(printf '%s\n' "${created_autostream_user_group_record}" |
    awk -F: 'NR == 1 && $1 == "autostream" { print $3 }')"
  [[ ${expected_group_gid} =~ ^[1-9][0-9]*$ &&
    $(printf '%s\n' "${created_autostream_user_record}" |
      awk -F: 'NR == 1 { print $4 }') == "${expected_group_gid}" &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  local_group_database_references_are_safe || return 1
  group_content_digest="$(calculate_local_group_database_digest /etc/group)" || return 1
  gshadow_content_digest="$(calculate_local_group_database_digest /etc/gshadow)" || return 1

  # Ubuntu enables USERGROUPS_ENAB, so userdel removes an empty same-name
  # primary group. Rename only the invocation-created user to a verified
  # collision-free login so userdel cannot select the autostream group.
  renamed_user_record="${AUTOSTREAM_USER_ROLLBACK_LOGIN}${user_record_suffix}"
  if usermod --login "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" autostream; then
    rename_status=0
  else
    rename_status=$?
  fi
  if [[ ${rename_status} -ne 0 ||
    -n $(getent passwd autostream 2>/dev/null || true) ||
    $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) != \
      "${renamed_user_record}" ||
    $(getent group autostream 2>/dev/null || true) != \
      "${created_autostream_user_group_record}" ||
    -n $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] ||
    ! local_group_database_digests_match \
      "${group_content_digest}" "${gshadow_content_digest}"; then
    restore_renamed_autostream_user \
      "${created_autostream_user_group_record}" "${renamed_user_record}" \
      "${group_content_digest}" "${gshadow_content_digest}" || :
    return 1
  fi

  if userdel "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"; then
    delete_status=0
  else
    delete_status=$?
  fi
  if [[ ${delete_status} -ne 0 ]]; then
    restore_renamed_autostream_user \
      "${created_autostream_user_group_record}" "${renamed_user_record}" \
      "${group_content_digest}" "${gshadow_content_digest}" || :
    return 1
  fi
  [[ -z $(getent passwd autostream 2>/dev/null || true) &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    $(getent group autostream 2>/dev/null || true) == \
      "${created_autostream_user_group_record}" &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  local_group_database_digests_match \
    "${group_content_digest}" "${gshadow_content_digest}" || return 1
  created_autostream_user=false
  created_autostream_user_record=""
}

rollback_created_autostream_account() {
  local rollback_failed=false
  if [[ ${created_autostream_user} == true ]]; then
    if [[ "$(getent passwd autostream || true)" != "${created_autostream_user_record}" ]]; then
      rollback_failed=true
    elif ! remove_created_autostream_user_preserving_group; then
      rollback_failed=true
    else
      created_autostream_user=false
    fi
  fi
  if [[ ${created_autostream_group} == true ]]; then
    if [[ ${created_autostream_user} == true ]]; then
      rollback_failed=true
    elif [[ "$(getent group autostream || true)" != "${created_autostream_group_record}" ]]; then
      rollback_failed=true
    elif ! groupdel autostream; then
      rollback_failed=true
    elif getent group autostream >/dev/null; then
      rollback_failed=true
    else
      created_autostream_group=false
    fi
  fi
  [[ ${rollback_failed} == false ]]
}
