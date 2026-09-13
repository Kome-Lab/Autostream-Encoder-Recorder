#!/bin/bash
set -euo pipefail

umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

readonly SERVICE_NAME="encoder-recorder"
readonly COMPONENT_NAME="encoder-recorder"
readonly AUTOSTREAM_USER_ROLLBACK_LOGIN="autostream-install-rollback"
readonly MANAGED_ROOT="/opt/autostream/encoder-recorder"
readonly RELEASES_DIR="${MANAGED_ROOT}/releases"
readonly CURRENT_LINK="${MANAGED_ROOT}/current"
readonly PUBLIC_BINARY="/usr/local/bin/autostream-encoder-recorder"
readonly PUBLIC_ALIAS="/usr/local/bin/encoder-recorder"
readonly ENV_DEST="/etc/autostream/encoder-recorder.env"
readonly UNIT_DEST="/etc/systemd/system/autostream-encoder-recorder.service"
readonly UNIT_NAME="autostream-encoder-recorder.service"
readonly STATE_DIR="/var/lib/autostream/encoder-recorder"
readonly ARCHIVE_DIR="/var/lib/autostream/archives"
readonly BACKUP_BASE="/var/backups/autostream"
readonly BACKUP_ROOT="${BACKUP_BASE}/install-migrations"
readonly BACKUP_DIR="${BACKUP_ROOT}/encoder-recorder"

die() {
  printf 'install-autostream-encoder-recorder: %s\n' "$*" >&2
  exit 1
}

[[ ${EUID} -eq 0 ]] || die "must run as root (use sudo)"
[[ $# -eq 0 ]] || die "this installer does not accept arguments"

for command in \
  awk basename chmod chown cmp cp dirname ffmpeg find flock getent grep groupadd groupdel id install jq \
  ln mktemp mv readlink rm rmdir runuser sha256sum sort stat sync systemctl tar uname uniq useradd userdel usermod wc; do
  command -v "${command}" >/dev/null 2>&1 || die "required command is unavailable: ${command}"
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
[[ ${SCRIPT_DIR} == /* && -d ${SCRIPT_DIR} ]] || die "could not resolve the installer directory"
readonly SCRIPT_DIR
ARTIFACT_ID="$(basename -- "${SCRIPT_DIR}")"
[[ -n ${ARTIFACT_ID} ]] || die "could not resolve the artifact directory name"
readonly ARTIFACT_ID
if [[ ! ${ARTIFACT_ID} =~ ^autostream-encoder-recorder_(v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?)_linux_(amd64|arm64)$ ]]; then
  die "the extracted directory name is invalid: ${ARTIFACT_ID}"
fi
readonly VERSION="${BASH_REMATCH[1]}"
readonly ARCH="${BASH_REMATCH[4]}"

case "$(uname -m)" in
  x86_64) readonly HOST_ARCH="amd64" ;;
  aarch64|arm64) readonly HOST_ARCH="arm64" ;;
  *) die "unsupported host architecture: $(uname -m)" ;;
esac
[[ ${ARCH} == "${HOST_ARCH}" ]] || die "artifact architecture ${ARCH} does not match host ${HOST_ARCH}"

ARTIFACT_PARENT="$(dirname -- "${SCRIPT_DIR}")"
[[ ${ARTIFACT_PARENT} == /* && -d ${ARTIFACT_PARENT} ]] || die "could not resolve the artifact parent directory"
readonly ARTIFACT_PARENT
readonly ARCHIVE_NAME="${ARTIFACT_ID}.tar.gz"
readonly ARCHIVE_SOURCE="${ARTIFACT_PARENT}/${ARCHIVE_NAME}"
[[ -f ${ARCHIVE_SOURCE} && ! -L ${ARCHIVE_SOURCE} ]] || \
  die "required release archive is missing or unsafe: ${ARCHIVE_SOURCE}"
ARCHIVE_SOURCE_SIZE="$(stat -c %s -- "${ARCHIVE_SOURCE}")"
[[ ${ARCHIVE_SOURCE_SIZE} =~ ^[1-9][0-9]*$ && ${ARCHIVE_SOURCE_SIZE} -le 268435456 ]] || \
  die "release archive size must be between 1 and 268435456 bytes"
readonly ARCHIVE_SOURCE_SIZE

cleanup_running=false
signal_transaction_active=false
deferred_termination_status=0

terminate_installer() {
  local status=$1
  trap '' HUP INT TERM
  exit "${status}"
}

handle_installer_signal() {
  local status=$1
  if [[ ${signal_transaction_active} == true ]]; then
    deferred_termination_status=${status}
    return
  fi
  terminate_installer "${status}"
}

restore_installer_signal_traps() {
  if [[ ${cleanup_running} == true ]]; then
    trap '' HUP INT TERM
  else
    trap 'handle_installer_signal 129' HUP
    trap 'handle_installer_signal 130' INT
    trap 'handle_installer_signal 143' TERM
  fi
}

begin_installer_signal_transaction() {
  [[ ${signal_transaction_active} == false ]] || \
    die "nested installer signal transaction is not allowed"
  signal_transaction_active=true
}

finish_installer_signal_transaction() {
  local pending_status
  signal_transaction_active=false
  pending_status=${deferred_termination_status}
  deferred_termination_status=0
  restore_installer_signal_traps
  if (( pending_status != 0 )); then
    terminate_installer "${pending_status}"
  fi
}

input_stage_is_owned() {
  [[ -d ${INPUT_STAGE} && ! -L ${INPUT_STAGE} &&
    $(readlink -f -- "${INPUT_STAGE}") == "${INPUT_STAGE}" &&
    $(stat -c '%d:%i' -- "${INPUT_STAGE}") == "${INPUT_STAGE_IDENTITY}" ]]
}

cleanup_early_input_stage() {
  local status=$?
  trap '' HUP INT TERM
  cleanup_running=true
  signal_transaction_active=false
  deferred_termination_status=0
  trap - EXIT
  if input_stage_is_owned; then
    rm -rf -- "${INPUT_STAGE}"
  else
    printf '%s\n' "install-autostream-encoder-recorder: refusing to remove replaced installer staging" >&2
    status=1
  fi
  exit "${status}"
}

# INPUT_STAGE is the single temporary-path journal exception: it is the
# root-only durable recovery container for every artifact kept after an
# incomplete rollback. A pending signal is delivered after its exact inode is
# captured and the early EXIT cleanup owns it.
INPUT_STAGE=""
INPUT_STAGE_IDENTITY=""
input_stage_status=0
restore_installer_signal_traps
begin_installer_signal_transaction
if INPUT_STAGE="$(mktemp -d /var/tmp/autostream-encoder-recorder-install.XXXXXXXX)"; then
  :
else
  input_stage_status=$?
fi
if [[ ${input_stage_status} -ne 0 ]]; then
  finish_installer_signal_transaction
  printf 'install-autostream-encoder-recorder: could not create the installer staging directory (mktemp status %s)\n' \
    "${input_stage_status}" >&2
  exit "${input_stage_status}"
fi
if [[ ! ( -n ${INPUT_STAGE} &&
  ${INPUT_STAGE} == /var/tmp/autostream-encoder-recorder-install.* &&
  -d ${INPUT_STAGE} && ! -L ${INPUT_STAGE} &&
  $(readlink -f -- "${INPUT_STAGE}") == "${INPUT_STAGE}" &&
  $(stat -c '%U:%G:%a' -- "${INPUT_STAGE}") == "root:root:700" ) ]]; then
  rm -rf -- "${INPUT_STAGE}"
  INPUT_STAGE=""
  finish_installer_signal_transaction
  die "could not create a safe installer staging directory"
fi
if ! INPUT_STAGE_IDENTITY="$(stat -c '%d:%i' -- "${INPUT_STAGE}")"; then
  rm -rf -- "${INPUT_STAGE}"
  INPUT_STAGE=""
  finish_installer_signal_transaction
  die "could not capture the installer staging identity"
fi
readonly INPUT_STAGE
readonly INPUT_STAGE_IDENTITY
readonly UNPACK_DIR="${INPUT_STAGE}/unpack"
trap cleanup_early_input_stage EXIT
finish_installer_signal_transaction

managed_candidate=""
managed_candidate_identity=""
current_next=""
unit_stage=""
env_stage=""
public_link_next=""
activation_started=false
installation_complete=false
previous_current_kind="absent"
previous_current_target=""
declare -A previous_public_kind=()
declare -A previous_public_target=()
declare -A previous_public_identity=()
declare -A previous_public_nlink=()
declare -A previous_public_mode=()
declare -A previous_public_sha256=()
declare -A public_rollback_anchor_path=()
declare -A public_rollback_anchor_owner=()
declare -A published_public_target=()
declare -A published_public_identity=()
unit_previous_kind="absent"
unit_previous_mode=""
unit_previous_sha256=""
unit_previous_backup="${INPUT_STAGE}/unit.previous"
unit_replaced=false
env_created=false
env_created_identity=""
rollback_incomplete=false
state_directory_snapshot_done=false
state_directory_previous_kind="unobserved"
state_directory_previous_uid=""
state_directory_previous_gid=""
state_directory_previous_mode=""
state_directory_previous_identity=""
state_directory_mutation_started=false
state_directory_created_identity=""
archive_directory_snapshot_done=false
archive_directory_previous_kind="unobserved"
archive_directory_previous_uid=""
archive_directory_previous_gid=""
archive_directory_previous_mode=""
archive_directory_previous_identity=""
archive_directory_mutation_started=false
archive_directory_created_identity=""
created_autostream_user=false
created_autostream_user_record=""
created_autostream_user_group_record=""
autostream_user_rollback_login_ready=false
created_autostream_group=false
created_autostream_group_record=""
release_created=false
release_created_identity=""
declare -a journaled_directory_order=()
declare -A journaled_directory_recorded=()
declare -A journaled_directory_previous_kind=()
declare -A journaled_directory_previous_identity=()
declare -A journaled_directory_previous_uid=()
declare -A journaled_directory_previous_gid=()
declare -A journaled_directory_previous_mode=()
declare -A journaled_directory_created_identity=()
declare -A temporary_path_identity=()
declare -A backup_previous_kind=()
declare -A backup_previous_identity=()
declare -A backup_previous_uid=()
declare -A backup_previous_gid=()
declare -A backup_previous_mode=()
declare -A backup_previous_sha256=()
declare -A backup_created_identity=()
