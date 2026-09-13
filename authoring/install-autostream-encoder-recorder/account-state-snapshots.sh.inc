
create_autostream_group_transactionally() {
  local create_status=0
  local created_record=""

  begin_installer_signal_transaction
  if groupadd --system autostream; then
    created_autostream_group=true
    if created_record="$(getent group autostream)" && [[ -n ${created_record} ]]; then
      created_autostream_group_record="${created_record}"
    else
      create_status=1
      if groupdel autostream && ! getent group autostream >/dev/null; then
        created_autostream_group=false
        created_autostream_group_record=""
      else
        rollback_incomplete=true
      fi
    fi
  else
    create_status=$?
  fi
  finish_installer_signal_transaction
  [[ ${create_status} -eq 0 ]]
}

create_autostream_user_transactionally() {
  local primary_gid=$1
  local create_status=0
  local created_record=""
  local protected_group_record=""

  [[ ${primary_gid} =~ ^[0-9]+$ &&
    ${primary_gid} -ne 0 &&
    ${autostream_user_rollback_login_ready} == true &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  if ! protected_group_record="$(getent group autostream)" || [[ -z ${protected_group_record} ]]; then
    return 1
  fi
  local_group_database_references_are_safe || return 1
  created_autostream_user_group_record="${protected_group_record}"
  begin_installer_signal_transaction
  if useradd --system --gid "${primary_gid}" --home-dir /var/lib/autostream \
    --no-create-home --shell /usr/sbin/nologin autostream; then
    created_autostream_user=true
    if created_record="$(getent passwd autostream)" && [[ -n ${created_record} ]]; then
      created_autostream_user_record="${created_record}"
    else
      create_status=1
      rollback_incomplete=true
    fi
  else
    create_status=$?
  fi
  finish_installer_signal_transaction
  [[ ${create_status} -eq 0 ]]
}

snapshot_existing_state_directory() {
  local existing_mode
  if [[ -e ${STATE_DIR} || -L ${STATE_DIR} ]]; then
    [[ -d ${STATE_DIR} && ! -L ${STATE_DIR} ]] || die "required directory path is unsafe: ${STATE_DIR}"
    [[ $(readlink -f -- "${STATE_DIR}") == "${STATE_DIR}" ]] || \
      die "required directory path is not canonical: ${STATE_DIR}"
    [[ $(stat -c '%U:%G' -- "${STATE_DIR}") == "autostream:autostream" ]] || \
      die "existing directory must be owned by autostream:autostream: ${STATE_DIR}"
    existing_mode="$(stat -c '%a' -- "${STATE_DIR}")"
    (( (8#${existing_mode} & 07022) == 0 )) || \
      die "existing directory has unsafe write or special mode bits: ${STATE_DIR}"
    state_directory_previous_kind="directory"
    state_directory_previous_uid="$(stat -c '%u' -- "${STATE_DIR}")"
    state_directory_previous_gid="$(stat -c '%g' -- "${STATE_DIR}")"
    state_directory_previous_mode="$(stat -c '%a' -- "${STATE_DIR}")"
    state_directory_previous_identity="$(stat -c '%d:%i' -- "${STATE_DIR}")"
  else
    state_directory_previous_kind="absent"
  fi
  state_directory_snapshot_done=true
}

snapshot_existing_archive_directory() {
  local existing_mode
  if [[ -e ${ARCHIVE_DIR} || -L ${ARCHIVE_DIR} ]]; then
    [[ -d ${ARCHIVE_DIR} && ! -L ${ARCHIVE_DIR} ]] || die "required directory path is unsafe: ${ARCHIVE_DIR}"
    [[ $(readlink -f -- "${ARCHIVE_DIR}") == "${ARCHIVE_DIR}" ]] || \
      die "required directory path is not canonical: ${ARCHIVE_DIR}"
    [[ $(stat -c '%U:%G' -- "${ARCHIVE_DIR}") == "autostream:autostream" ]] || \
      die "existing directory must be owned by autostream:autostream: ${ARCHIVE_DIR}"
    existing_mode="$(stat -c '%a' -- "${ARCHIVE_DIR}")"
    (( (8#${existing_mode} & 07022) == 0 )) || \
      die "existing directory has unsafe write or special mode bits: ${ARCHIVE_DIR}"
    archive_directory_previous_kind="directory"
    archive_directory_previous_uid="$(stat -c '%u' -- "${ARCHIVE_DIR}")"
    archive_directory_previous_gid="$(stat -c '%g' -- "${ARCHIVE_DIR}")"
    archive_directory_previous_mode="$(stat -c '%a' -- "${ARCHIVE_DIR}")"
    archive_directory_previous_identity="$(stat -c '%d:%i' -- "${ARCHIVE_DIR}")"
  else
    archive_directory_previous_kind="absent"
  fi
  archive_directory_snapshot_done=true
}
