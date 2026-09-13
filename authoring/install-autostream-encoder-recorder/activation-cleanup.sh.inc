
rollback_activation() {
  local rollback_failed=false
  if ! restore_unit_state; then
    printf '%s\n' "install-autostream-encoder-recorder: failed to restore the previous systemd unit" >&2
    rollback_failed=true
  fi
  if [[ ${env_created} == true ]]; then
    if [[ -e ${ENV_DEST} || -L ${ENV_DEST} ]]; then
      if [[ -f ${ENV_DEST} && ! -L ${ENV_DEST} &&
        -n ${env_created_identity} &&
        $(stat -c '%d:%i' -- "${ENV_DEST}") == "${env_created_identity}" ]]; then
        rm -f -- "${ENV_DEST}" || rollback_failed=true
        if [[ -e ${ENV_DEST} || -L ${ENV_DEST} ]]; then
          rollback_failed=true
        fi
      else
        printf '%s\n' "install-autostream-encoder-recorder: refused to remove an unexpected environment path during rollback" >&2
        rollback_failed=true
      fi
    fi
    if ! sync -f "$(dirname -- "${ENV_DEST}")"; then
      rollback_failed=true
    fi
  fi
  if ! restore_public_state "${PUBLIC_BINARY}"; then
    printf '%s\n' "install-autostream-encoder-recorder: failed to restore the previous canonical public binary" >&2
    rollback_failed=true
  fi
  if ! restore_public_state "${PUBLIC_ALIAS}"; then
    printf '%s\n' "install-autostream-encoder-recorder: failed to restore the previous public alias" >&2
    rollback_failed=true
  fi
  if ! restore_current_state; then
    printf '%s\n' "install-autostream-encoder-recorder: failed to restore the previous current link" >&2
    rollback_failed=true
  fi
  if [[ ${unit_replaced} == true ]] && ! systemctl daemon-reload; then
    printf '%s\n' "install-autostream-encoder-recorder: failed to reload restored systemd state" >&2
    rollback_failed=true
  fi
  if [[ ${rollback_failed} == true ]]; then
    rollback_incomplete=true
    printf '%s\n' "install-autostream-encoder-recorder: rollback was incomplete; inspect the host before retrying" >&2
    return 1
  fi
  printf '%s\n' "install-autostream-encoder-recorder: restored the pre-install activation state" >&2
}

observe_path_identity_and_nlink() {
  local path=$1
  if [[ -e ${path} || -L ${path} ]]; then
    stat -c '%d:%i:%h' -- "${path}" 2>/dev/null || printf '%s\n' unreadable
  else
    printf '%s\n' absent
  fi
}

cleanup() {
  local status=$?
  local setup_rollback_safe=true
  local managed_cleanup_identity="${managed_candidate_identity}"
  # Rollback is non-reentrant. A second signal must not interrupt restoration
  # after the EXIT trap has been removed.
  trap '' HUP INT TERM
  cleanup_running=true
  signal_transaction_active=false
  deferred_termination_status=0
  trap - EXIT
  set +e
  if [[ ${status} -ne 0 && ${activation_started} == true && ${installation_complete} != true ]]; then
    if ! rollback_activation; then
      setup_rollback_safe=false
    fi
  fi
  if [[ ${status} -ne 0 && ${state_directory_mutation_started} == true &&
    ${installation_complete} != true ]]; then
    if ! restore_existing_state_directory; then
      printf '%s\n' "install-autostream-encoder-recorder: failed to restore the previous state directory metadata" >&2
      rollback_incomplete=true
      setup_rollback_safe=false
    fi
  fi
  if [[ ${status} -ne 0 && ${archive_directory_mutation_started} == true &&
    ${installation_complete} != true ]]; then
    if ! restore_existing_archive_directory; then
      printf '%s\n' "install-autostream-encoder-recorder: failed to restore the previous archive directory metadata" >&2
      rollback_incomplete=true
      setup_rollback_safe=false
    fi
  fi
  if [[ -n ${managed_candidate} && ( -e ${managed_candidate} || -L ${managed_candidate} ) ]]; then
    if [[ -z ${managed_cleanup_identity} ]]; then
      managed_cleanup_identity="${temporary_path_identity["${managed_candidate}"]-}"
    fi
    if [[ -d ${managed_candidate} && ! -L ${managed_candidate} &&
      -n ${managed_cleanup_identity} &&
      ${managed_cleanup_identity} != "pending" &&
      $(stat -c '%d:%i' -- "${managed_candidate}") == "${managed_cleanup_identity}" ]]; then
      rm -rf -- "${managed_candidate}" || {
        rollback_incomplete=true
        setup_rollback_safe=false
      }
    else
      rollback_incomplete=true
      setup_rollback_safe=false
    fi
  fi
  managed_candidate=""
  managed_candidate_identity=""
  if [[ ${installation_complete} == true ]]; then
    if ! finalize_public_rollback_anchors; then
      printf '%s\n' "install-autostream-encoder-recorder: failed to finalize a public rollback anchor" >&2
      rollback_incomplete=true
      setup_rollback_safe=false
    fi
  fi
  if ! cleanup_temporary_paths; then
    printf '%s\n' "install-autostream-encoder-recorder: failed to clean an invocation-owned temporary path" >&2
    rollback_incomplete=true
    setup_rollback_safe=false
  fi
  if [[ ${status} -ne 0 && ${installation_complete} != true &&
    ${setup_rollback_safe} == true && ${release_created} == true ]]; then
    if ! rollback_created_release; then
      printf '%s\n' "install-autostream-encoder-recorder: failed to remove the invocation-created release" >&2
      rollback_incomplete=true
      setup_rollback_safe=false
    fi
  fi
  if [[ ${status} -ne 0 && ${installation_complete} != true ]]; then
    if ! rollback_journaled_directories; then
      printf '%s\n' "install-autostream-encoder-recorder: failed to restore invocation-mutated directories" >&2
      rollback_incomplete=true
      setup_rollback_safe=false
    fi
  fi
  if [[ ${status} -ne 0 && ${installation_complete} != true &&
    ${setup_rollback_safe} == true ]]; then
    if ! rollback_created_autostream_account; then
      printf '%s\n' "install-autostream-encoder-recorder: failed to remove the invocation-created service account" >&2
      rollback_incomplete=true
    fi
  fi
  if [[ ${rollback_incomplete} == true && ${status} -eq 0 ]]; then
    status=1
  fi
  if [[ ${rollback_incomplete} == true ]]; then
    recovery_evidence_ready=true
    if ! {
      printf 'service=%s\n' "${SERVICE_NAME}"
      printf 'previous_current_kind=%s\n' "${previous_current_kind}"
      printf 'previous_current_target=%s\n' "${previous_current_target}"
      printf 'public_binary_kind=%s\n' "${previous_public_kind["${PUBLIC_BINARY}"]-unknown}"
      printf 'public_binary_target=%s\n' "${previous_public_target["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_mode=%s\n' "${previous_public_mode["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_sha256=%s\n' "${previous_public_sha256["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_previous_identity=%s\n' "${previous_public_identity["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_previous_nlink=%s\n' "${previous_public_nlink["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_anchor_path=%s\n' "${public_rollback_anchor_path["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_anchor_observed=%s\n' \
        "$(observe_path_identity_and_nlink "${public_rollback_anchor_path["${PUBLIC_BINARY}"]-}")"
      printf 'public_binary_published_identity=%s\n' "${published_public_identity["${PUBLIC_BINARY}"]-}"
      printf 'public_binary_published_target=%s\n' "${published_public_target["${PUBLIC_BINARY}"]-}"
      printf 'public_alias_kind=%s\n' "${previous_public_kind["${PUBLIC_ALIAS}"]-unknown}"
      printf 'public_alias_target=%s\n' "${previous_public_target["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_mode=%s\n' "${previous_public_mode["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_sha256=%s\n' "${previous_public_sha256["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_previous_identity=%s\n' "${previous_public_identity["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_previous_nlink=%s\n' "${previous_public_nlink["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_anchor_path=%s\n' "${public_rollback_anchor_path["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_anchor_observed=%s\n' \
        "$(observe_path_identity_and_nlink "${public_rollback_anchor_path["${PUBLIC_ALIAS}"]-}")"
      printf 'public_alias_published_identity=%s\n' "${published_public_identity["${PUBLIC_ALIAS}"]-}"
      printf 'public_alias_published_target=%s\n' "${published_public_target["${PUBLIC_ALIAS}"]-}"
      printf 'unit_previous_kind=%s\n' "${unit_previous_kind}"
      printf 'unit_previous_mode=%s\n' "${unit_previous_mode}"
      printf 'unit_previous_sha256=%s\n' "${unit_previous_sha256}"
    } > "${INPUT_STAGE}/recovery-state.txt"; then
      recovery_evidence_ready=false
    fi
    chown -R root:root "${INPUT_STAGE}" || recovery_evidence_ready=false
    chmod -R go-rwx "${INPUT_STAGE}" || recovery_evidence_ready=false
    if [[ -f ${unit_previous_backup} && ! -L ${unit_previous_backup} ]]; then
      sync -f "${unit_previous_backup}" || recovery_evidence_ready=false
    fi
    if [[ -f ${INPUT_STAGE}/recovery-state.txt && ! -L ${INPUT_STAGE}/recovery-state.txt ]]; then
      sync -f "${INPUT_STAGE}/recovery-state.txt" || recovery_evidence_ready=false
    else
      recovery_evidence_ready=false
    fi
    sync -f "${INPUT_STAGE}" || recovery_evidence_ready=false
    if [[ ${recovery_evidence_ready} == true ]]; then
      printf 'install-autostream-encoder-recorder: root-only recovery evidence preserved at %s\n' "${INPUT_STAGE}" >&2
    else
      printf 'install-autostream-encoder-recorder: WARNING: rollback was incomplete and recovery staging could not be fully sealed; inspect %s immediately\n' "${INPUT_STAGE}" >&2
    fi
  else
    if input_stage_is_owned; then
      rm -rf -- "${INPUT_STAGE}"
    else
      printf '%s\n' "install-autostream-encoder-recorder: refusing to remove replaced installer staging" >&2
      status=1
    fi
  fi
  exit "${status}"
}
trap cleanup EXIT
restore_installer_signal_traps
install -d -o root -g root -m 0700 "${UNPACK_DIR}"
