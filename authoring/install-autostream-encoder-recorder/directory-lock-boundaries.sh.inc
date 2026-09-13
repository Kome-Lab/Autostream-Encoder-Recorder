
ensure_directory() {
  local path=$1
  local owner=$2
  local group=$3
  local mode=$4
  local existing_mode
  local existing_owner_group
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} ]] || die "required directory path is unsafe: ${path}"
    [[ $(readlink -f -- "${path}") == "${path}" ]] || die "required directory path is not canonical: ${path}"
    existing_owner_group="$(stat -c '%U:%G' -- "${path}")"
    [[ ${existing_owner_group} == "${owner}:${group}" ]] || \
      die "existing directory must be owned by ${owner}:${group}: ${path}"
    existing_mode="$(stat -c '%a' -- "${path}")"
    (( (8#${existing_mode} & 07022) == 0 )) || \
      die "existing directory has unsafe write or special mode bits: ${path}"
  fi
  journal_directory_before_mutation "${path}" || die "could not journal directory before mutation: ${path}"
  install_journaled_directory "${path}" "${owner}" "${group}" "${mode}" || \
    die "could not create or normalize journaled directory: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || die "required directory path is not canonical: ${path}"
  [[ $(stat -c '%U:%G:%a' -- "${path}") == "${owner}:${group}:${mode#0}" ]] || \
    die "directory ownership or mode normalization failed: ${path}"
}

ensure_managed_directory() {
  local path=$1
  local mode
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} ]] || die "managed directory path is unsafe: ${path}"
    [[ $(readlink -f -- "${path}") == "${path}" ]] || die "managed directory path is not canonical: ${path}"
    [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
      die "existing managed directory must be owned by root:root: ${path}"
    mode="$(stat -c '%a' -- "${path}")"
    (( (8#${mode} & 07022) == 0 )) || \
      die "existing managed directory has unsafe write or special mode bits: ${path}"
  fi
  journal_directory_before_mutation "${path}" || die "could not journal managed directory: ${path}"
  install_journaled_directory "${path}" root root 0755 || \
    die "could not create or normalize managed directory: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || die "managed directory path is not canonical: ${path}"
  [[ $(stat -c '%U:%G:%a' -- "${path}") == "root:root:755" ]] || \
    die "managed directory ownership or mode normalization failed: ${path}"
}

ensure_private_root_directory() {
  local path=$1
  local mode
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} ]] || die "private root directory path is unsafe: ${path}"
    [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
      die "existing private directory must be owned by root:root: ${path}"
    mode="$(stat -c '%a' -- "${path}")"
    (( (8#${mode} & 07077) == 0 )) || \
      die "existing private directory has unsafe group/other or special mode bits: ${path}"
  fi
  journal_directory_before_mutation "${path}" || die "could not journal private root directory: ${path}"
  install_journaled_directory "${path}" root root 0700 || \
    die "could not create or normalize private root directory: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || die "private root directory path is not canonical: ${path}"
  [[ $(stat -c '%U:%G:%a' -- "${path}") == "root:root:700" ]] || \
    die "private root directory ownership or mode normalization failed: ${path}"
}

ensure_root_anchor_directory() {
  local path=$1
  local mode
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} ]] || die "root anchor directory path is unsafe: ${path}"
    [[ $(readlink -f -- "${path}") == "${path}" ]] || die "root anchor directory path is not canonical: ${path}"
    [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
      die "root anchor directory must be owned by root:root: ${path}"
    mode="$(stat -c '%a' -- "${path}")"
    (( (8#${mode} & 07022) == 0 )) || \
      die "root anchor directory has unsafe write or special mode bits: ${path}"
    journal_directory_before_mutation "${path}" || die "could not journal root anchor directory: ${path}"
    return
  fi
  journal_directory_before_mutation "${path}" || die "could not journal root anchor directory: ${path}"
  install_journaled_directory "${path}" root root 0755 || \
    die "could not create root anchor directory: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || die "root anchor directory path is not canonical: ${path}"
  [[ $(stat -c '%U:%G:%a' -- "${path}") == "root:root:755" ]] || \
    die "root anchor directory ownership or mode normalization failed: ${path}"
}

ensure_permanent_lock_directory() {
  local path=$1
  local mode
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} &&
      $(readlink -f -- "${path}") == "${path}" &&
      $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
      die "permanent updater lock directory is unsafe: ${path}"
    mode="$(stat -c '%a' -- "${path}")"
    (( (8#${mode} & 07077) == 0 )) || \
      die "permanent updater lock directory grants unsafe permissions: ${path}"
  fi
  # This permanent root-only directory and its lock files intentionally survive
  # failed installs so every future invocation serializes on the same inode.
  install -d -o root -g root -m 0700 "${path}"
  [[ -d ${path} && ! -L ${path} &&
    $(readlink -f -- "${path}") == "${path}" &&
    $(stat -c '%U:%G:%a' -- "${path}") == "root:root:700" ]] || \
    die "permanent updater lock directory normalization failed: ${path}"
}

ensure_permanent_lock_path_atomically() {
  local path=$1
  local parent
  local lock_create_stage=""
  local lock_mode

  parent="$(dirname -- "${path}")" || die "could not resolve the permanent updater lock parent"
  [[ ${parent} == "/run/autostream-updater" ]] || \
    die "permanent updater lock parent is outside the private lock directory"
  if [[ ! -e ${path} && ! -L ${path} ]]; then
    create_registered_temporary_path lock_create_stage file \
      "${parent}/.lock-create.XXXXXXXX" || \
      die "could not create and journal a permanent updater lock candidate"
    chown root:root "${lock_create_stage}"
    chmod 0600 "${lock_create_stage}"
    [[ -f ${lock_create_stage} && ! -L ${lock_create_stage} &&
      $(stat -c '%U:%G:%a' -- "${lock_create_stage}") == "root:root:600" ]] || \
      die "permanent updater lock candidate is unsafe"
    if ln -- "${lock_create_stage}" "${path}" 2>/dev/null; then
      sync -f "${parent}"
    elif [[ ! -e ${path} && ! -L ${path} ]]; then
      die "could not atomically create a permanent updater lock"
    fi
    rm -f -- "${lock_create_stage}" || \
      die "could not remove the permanent updater lock candidate"
    forget_temporary_path "${lock_create_stage}"
  fi
  [[ -f ${path} && ! -L ${path} ]] || \
    die "permanent updater lock path is unsafe: ${path}"
  [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
    die "permanent updater lock must be owned by root:root: ${path}"
  lock_mode="$(stat -c '%a' -- "${path}")"
  (( (8#${lock_mode} & 0077) == 0 )) || \
    die "permanent updater lock must not grant group/other permissions: ${path}"
}
