
preflight_public_path() {
  local link_path=$1
  local expected_target=$2
  local parent
  local mode
  local previous_kind
  local source_digest
  local source_identity
  local source_nlink
  local source_target
  local backup_name
  local backup_path
  backup_name="$(basename -- "${link_path}")" || die "could not resolve the legacy public binary name"
  [[ -n ${backup_name} && ${backup_name} != */* ]] || die "legacy public binary name is unsafe"
  backup_path="${LEGACY_BACKUP_DIR}/${backup_name}"
  parent="$(dirname -- "${link_path}")"
  [[ -d ${parent} && ! -L ${parent} ]] || die "public path parent is missing or unsafe: ${parent}"
  previous_kind="${previous_public_kind["${link_path}"]-unobserved}"
  if [[ -L ${link_path} ]]; then
    source_target="$(readlink -- "${link_path}")"
    source_identity="$(stat -c '%d:%i' -- "${link_path}")"
    [[ ${source_target} == "${expected_target}" ]] || \
      die "existing public symlink has an unexpected target: ${link_path}"
    [[ ${source_identity} =~ ^[0-9]+:[0-9]+$ ]] || \
      die "could not capture the public symlink identity: ${link_path}"
    case "${previous_kind}" in
      unobserved)
        previous_public_kind["${link_path}"]="symlink"
        previous_public_target["${link_path}"]="${source_target}"
        previous_public_identity["${link_path}"]="${source_identity}"
        ;;
      symlink)
        [[ ${source_target} == "${previous_public_target["${link_path}"]}" &&
          ${source_identity} == "${previous_public_identity["${link_path}"]}" ]] || \
          die "public symlink changed after preflight: ${link_path}"
        ;;
      *)
        die "public path type changed after preflight: ${link_path}"
        ;;
    esac
    if [[ -z ${backup_previous_kind["${link_path}"]+present} ]]; then
      backup_previous_kind["${link_path}"]="not-applicable"
    fi
  elif [[ -e ${link_path} ]]; then
    [[ -f ${link_path} ]] || die "existing public path is not a regular file: ${link_path}"
    [[ $(stat -c '%U:%G' -- "${link_path}") == "root:root" ]] || \
      die "legacy public binary must be owned by root:root: ${link_path}"
    [[ -x ${link_path} ]] || die "legacy public binary is not executable: ${link_path}"
    mode="$(stat -c '%a' -- "${link_path}")"
    (( (8#${mode} & 07022) == 0 )) || \
      die "legacy public binary has unsafe write or special mode bits: ${link_path}"
    source_identity="$(stat -c '%d:%i' -- "${link_path}")"
    source_nlink="$(stat -c '%h' -- "${link_path}")"
    [[ ${source_identity} =~ ^[0-9]+:[0-9]+$ ]] || \
      die "could not capture the legacy public binary identity: ${link_path}"
    [[ ${source_nlink} =~ ^[1-9][0-9]*$ ]] || \
      die "could not capture the legacy public binary link count: ${link_path}"
    source_digest="$(sha256sum -- "${link_path}" | awk 'NR == 1 { print $1 }')"
    case "${previous_kind}" in
      unobserved)
        previous_public_kind["${link_path}"]="regular"
        previous_public_identity["${link_path}"]="${source_identity}"
        previous_public_nlink["${link_path}"]="${source_nlink}"
        previous_public_mode["${link_path}"]="${mode}"
        previous_public_sha256["${link_path}"]="${source_digest}"
        ;;
      regular)
        [[ ${source_identity} == "${previous_public_identity["${link_path}"]}" &&
          ${source_nlink} == "${previous_public_nlink["${link_path}"]}" &&
          ${mode} == "${previous_public_mode["${link_path}"]}" &&
          ${source_digest} == "${previous_public_sha256["${link_path}"]}" ]] || \
          die "legacy public binary changed after preflight: ${link_path}"
        ;;
      *)
        die "public path type changed after preflight: ${link_path}"
        ;;
    esac
    if [[ -z ${backup_previous_kind["${link_path}"]+present} ]]; then
      if [[ -e ${backup_path} || -L ${backup_path} ]]; then
        [[ -f ${backup_path} && ! -L ${backup_path} &&
          $(stat -c '%U:%G:%a' -- "${backup_path}") == "root:root:500" &&
          $(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }') == "${source_digest}" ]] || \
          die "legacy backup destination conflicts with ${link_path}"
        backup_previous_kind["${link_path}"]="regular"
        backup_previous_identity["${link_path}"]="$(stat -c '%d:%i' -- "${backup_path}")"
        backup_previous_uid["${link_path}"]="$(stat -c '%u' -- "${backup_path}")"
        backup_previous_gid["${link_path}"]="$(stat -c '%g' -- "${backup_path}")"
        backup_previous_mode["${link_path}"]="$(stat -c '%a' -- "${backup_path}")"
        backup_previous_sha256["${link_path}"]="${source_digest}"
      else
        backup_previous_kind["${link_path}"]="absent"
      fi
    else
      case "${backup_previous_kind["${link_path}"]}" in
        regular)
          [[ -f ${backup_path} && ! -L ${backup_path} &&
            $(stat -c '%d:%i' -- "${backup_path}") == "${backup_previous_identity["${link_path}"]}" &&
            $(stat -c '%u:%g:%a' -- "${backup_path}") == \
              "${backup_previous_uid["${link_path}"]}:${backup_previous_gid["${link_path}"]}:${backup_previous_mode["${link_path}"]}" &&
            $(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }') == \
              "${backup_previous_sha256["${link_path}"]}" &&
            ${backup_previous_sha256["${link_path}"]} == "${source_digest}" ]] || \
            die "legacy backup destination changed after preflight: ${link_path}"
          ;;
        absent)
          [[ ! -e ${backup_path} && ! -L ${backup_path} ]] || \
            die "legacy backup destination appeared after preflight: ${link_path}"
          ;;
        *)
          die "legacy backup state was not captured before mutation: ${link_path}"
          ;;
      esac
    fi
  else
    case "${previous_kind}" in
      unobserved)
        previous_public_kind["${link_path}"]="absent"
        ;;
      absent)
        ;;
      *)
        die "public path disappeared after preflight: ${link_path}"
        ;;
    esac
    if [[ -z ${backup_previous_kind["${link_path}"]+present} ]]; then
      backup_previous_kind["${link_path}"]="not-applicable"
    fi
  fi
}

reject_stale_public_rollback_anchors() {
  local path=$1
  local restore_nullglob=false
  local -a stale_anchors=()
  if ! shopt -q nullglob; then
    shopt -s nullglob
    restore_nullglob=true
  fi
  stale_anchors=("${path}.rollback-anchor."*)
  if [[ ${restore_nullglob} == true ]]; then
    shopt -u nullglob
  fi
  ((${#stale_anchors[@]} == 0)) || \
    die "stale public rollback anchor requires manual recovery: ${path}"
}

[[ -d /run && ! -L /run && $(readlink -f -- /run) == "/run" &&
  $(stat -c '%U:%G' -- /run) == "root:root" ]] || \
  die "permanent updater lock root is unsafe"
run_mode="$(stat -c '%a' -- /run)"
(( (8#${run_mode} & 07022) == 0 )) || \
  die "permanent updater lock root has unsafe write or special mode bits"
ensure_permanent_lock_directory /run/autostream-updater

# Service installers share account and parent-directory mutations, so this
# permanent runtime host-setup lock is held through success or rollback. Like
# the target lock below, it is never unlinked by an installer.
readonly SHARED_HOST_SETUP_LOCK="/run/autostream-updater/.autostream-runtime-host-setup.lock"
ensure_permanent_lock_path_atomically "${SHARED_HOST_SETUP_LOCK}"
exec 8<>"${SHARED_HOST_SETUP_LOCK}"
readonly SHARED_HOST_SETUP_LOCK_FD_PATH="/proc/self/fd/8"
chown root:root "${SHARED_HOST_SETUP_LOCK_FD_PATH}"
chmod 0600 "${SHARED_HOST_SETUP_LOCK_FD_PATH}"
shared_host_setup_lock_fd_identity="$(stat -Lc '%d:%i' -- "${SHARED_HOST_SETUP_LOCK_FD_PATH}")"
[[ -f ${SHARED_HOST_SETUP_LOCK} && ! -L ${SHARED_HOST_SETUP_LOCK} &&
  -f ${SHARED_HOST_SETUP_LOCK_FD_PATH} &&
  $(stat -Lc '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK_FD_PATH}") == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
  ${shared_host_setup_lock_fd_identity} == "$(stat -c '%d:%i' -- "${SHARED_HOST_SETUP_LOCK}")" ]] || \
  die "shared host-setup lock identity changed while being opened"
flock -n 8 || die "another AutoStream installer is provisioning shared host state"
[[ -f ${SHARED_HOST_SETUP_LOCK} && ! -L ${SHARED_HOST_SETUP_LOCK} &&
  -f ${SHARED_HOST_SETUP_LOCK_FD_PATH} &&
  $(stat -Lc '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK_FD_PATH}") == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
  ${shared_host_setup_lock_fd_identity} == "$(stat -c '%d:%i' -- "${SHARED_HOST_SETUP_LOCK}")" ]] || \
  die "shared host-setup lock identity changed after acquisition"

ensure_root_anchor_directory /usr
ensure_root_anchor_directory /usr/local
ensure_root_anchor_directory /usr/local/bin
ensure_root_anchor_directory /etc
ensure_root_anchor_directory /etc/systemd
ensure_root_anchor_directory /etc/systemd/system
ensure_root_anchor_directory /opt
ensure_root_anchor_directory /var
ensure_root_anchor_directory /var/lib
ensure_root_anchor_directory /var/backups
ensure_root_anchor_directory /run
TARGET_LOCK_ID="$(printf '%s' "${UNIT_NAME}" | sha256sum | awk 'NR == 1 { print substr($1, 1, 12) }')"
[[ ${TARGET_LOCK_ID} =~ ^[0-9a-f]{12}$ ]] || die "could not derive the updater target lock identifier"
readonly TARGET_LOCK_ID
readonly TARGET_LOCK="/run/autostream-updater/.autostream-updater-${TARGET_LOCK_ID}.lock"
ensure_permanent_lock_path_atomically "${TARGET_LOCK}"
exec 9<>"${TARGET_LOCK}"
readonly TARGET_LOCK_FD_PATH="/proc/self/fd/9"
chown root:root "${TARGET_LOCK_FD_PATH}"
chmod 0600 "${TARGET_LOCK_FD_PATH}"
target_lock_fd_identity="$(stat -Lc '%d:%i' -- "${TARGET_LOCK_FD_PATH}")"
[[ -f ${TARGET_LOCK} && ! -L ${TARGET_LOCK} &&
  -f ${TARGET_LOCK_FD_PATH} &&
  $(stat -Lc '%U:%G:%a' -- "${TARGET_LOCK_FD_PATH}") == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" &&
  ${target_lock_fd_identity} == "$(stat -c '%d:%i' -- "${TARGET_LOCK}")" ]] || \
  die "updater lock descriptor/path identity changed while being opened"
flock -n 9 || die "another privileged update is already active for ${UNIT_NAME}"
[[ -f ${TARGET_LOCK} && ! -L ${TARGET_LOCK} &&
  -f ${TARGET_LOCK_FD_PATH} &&
  $(stat -Lc '%U:%G:%a' -- "${TARGET_LOCK_FD_PATH}") == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" &&
  ${target_lock_fd_identity} == "$(stat -c '%d:%i' -- "${TARGET_LOCK}")" ]] || \
  die "updater lock descriptor/path identity changed after acquisition"

preflight_public_path "${PUBLIC_BINARY}" "${CURRENT_LINK}/bin/autostream-encoder-recorder"
preflight_public_path "${PUBLIC_ALIAS}" "${PUBLIC_BINARY}"
if [[ ${previous_public_kind["${PUBLIC_BINARY}"]} == "regular" &&
  ${previous_public_kind["${PUBLIC_ALIAS}"]} == "regular" &&
  ${previous_public_identity["${PUBLIC_BINARY}"]} == \
    "${previous_public_identity["${PUBLIC_ALIAS}"]}" ]]; then
  die "legacy canonical public binary and alias must not be hard links to the same inode"
fi
reject_stale_public_rollback_anchors "${PUBLIC_BINARY}"
reject_stale_public_rollback_anchors "${PUBLIC_ALIAS}"
if [[ ${previous_public_kind["${PUBLIC_BINARY}"]} == "absent" ]] &&
  systemctl is-active --quiet "${UNIT_NAME}"; then
  die "active service has no canonical public binary; stop it before installing"
fi
snapshot_existing_state_directory
snapshot_existing_archive_directory

if ! id autostream >/dev/null 2>&1; then
  prepare_autostream_user_rollback_login || \
    die "could not prepare a safe service-account rollback login"
fi
if ! getent group autostream >/dev/null; then
  create_autostream_group_transactionally || \
    die "could not create and journal the autostream service group"
fi
autostream_group_gid="$(getent group autostream | awk -F: 'NR == 1 { print $3 }')"
[[ ${autostream_group_gid} =~ ^[0-9]+$ && ${autostream_group_gid} -ne 0 ]] || \
  die "autostream service group must have a non-root numeric gid"
if ! id autostream >/dev/null 2>&1; then
  create_autostream_user_transactionally "${autostream_group_gid}" || \
    die "could not create and journal the autostream service account"
fi
[[ $(id -u autostream) -ne 0 ]] || die "autostream service account must not be root"
[[ $(id -g autostream) == "${autostream_group_gid}" ]] || \
  die "autostream service account must use the verified autostream group"
[[ $(id -gn autostream) == "autostream" ]] || die "autostream service account must use the autostream primary group"

ensure_managed_directory /opt/autostream
ensure_managed_directory "${MANAGED_ROOT}"
ensure_managed_directory "${RELEASES_DIR}"
ensure_directory /var/lib/autostream root root 0755
ensure_private_root_directory "${BACKUP_BASE}"
ensure_private_root_directory "${BACKUP_ROOT}"
ensure_private_root_directory "${BACKUP_DIR}"
ensure_directory /etc/autostream root root 0750

readonly RELEASE_DIR="${RELEASES_DIR}/${VERSION}-${ARTIFACT_SHA256:0:12}"
