#!/bin/bash
set -euo pipefail

umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

die() {
  printf 'encoder-recorder installer integration test: %s\n' "$*" >&2
  exit 1
}

assert_not_enabled() {
  if systemctl is-enabled --quiet "${UNIT}"; then
    unit_enable_cleanup_needed=true
    die "installer unexpectedly enabled ${UNIT}"
  fi
  return 0
}

[[ ${EUID} -eq 0 ]] || die "must run as root"
[[ $(uname -m) == "x86_64" ]] || die "this integration fixture requires an amd64 Linux runner"

if [[ ${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS:-} != "1" ]]; then
  exec unshare --mount --propagation private bash -c '
    set -euo pipefail
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-scratch /mnt
    install -d -o root -g root -m 0755 /mnt/usr-lower
    mount --rbind /usr /mnt/usr-lower
    mount --make-rprivate /mnt/usr-lower
    install -d -o root -g root -m 0755 /mnt/etc-lower
    mount --rbind /etc /mnt/etc-lower
    mount --make-rprivate /mnt/etc-lower
    install -d -o root -g root -m 0755 /mnt/var-lower
    mount --rbind /var /mnt/var-lower
    mount --make-rprivate /mnt/var-lower
    install -d -o root -g root -m 0755 /mnt/run-lower
    mount --rbind /run /mnt/run-lower
    mount --make-rprivate /mnt/run-lower
    install -d -o root -g root -m 0755 /mnt/usr-upper
    install -d -o root -g root -m 0755 /mnt/usr-upper/local
    install -d -o root -g root -m 0755 \
      /mnt/etc-upper \
      /mnt/etc-upper/systemd \
      /mnt/etc-upper/systemd/system \
      /mnt/var-upper \
      /mnt/var-upper/lib \
      /mnt/var-upper/backups \
      /mnt/run-upper
    install -d -o root -g root -m 1777 /mnt/var-upper/tmp
    install -d -o root -g root -m 0700 \
      /mnt/usr-work \
      /mnt/etc-work \
      /mnt/var-work \
      /mnt/run-work
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/usr-lower,upperdir=/mnt/usr-upper,workdir=/mnt/usr-work \
      autostream-encoder-recorder-installer-test-usr-overlay /usr
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/etc-lower,upperdir=/mnt/etc-upper,workdir=/mnt/etc-work \
      autostream-encoder-recorder-installer-test-etc-overlay /etc
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/var-lower,upperdir=/mnt/var-upper,workdir=/mnt/var-work \
      autostream-encoder-recorder-installer-test-var-overlay /var
    mount -t overlay \
      -o nodev,nosuid,lowerdir=/mnt/run-lower,upperdir=/mnt/run-upper,workdir=/mnt/run-work \
      autostream-encoder-recorder-installer-test-run-overlay /run
    host_run_systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"
    mount --rbind /mnt/run-lower/systemd /run/systemd
    mount --make-rprivate /run/systemd
    [[ $(stat -c "%d:%i" -- /run/systemd) == "${host_run_systemd_identity}" ]]
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-bin /usr/local/bin
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-opt /opt
    mount -t tmpfs -o ro,nodev,nosuid,noexec,mode=0555,uid=0,gid=0 \
      autostream-encoder-recorder-installer-test-sealed-mnt /mnt
    exec env \
      AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_MOUNT_NS=1 \
      AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_RUN_SYSTEMD_ID="${host_run_systemd_identity}" \
      bash "$1"
  ' autostream-encoder-recorder-installer-test-mount "$0"
fi

assert_sealed_scratch_mount() {
  local probe="/mnt/.autostream-installer-write-probe"

  awk '
    $5 == "/mnt" {
      has_ro = 0
      has_nodev = 0
      has_nosuid = 0
      has_noexec = 0
      option_count = split($6, options, ",")
      for (option = 1; option <= option_count; option++) {
        has_ro = has_ro || options[option] == "ro"
        has_nodev = has_nodev || options[option] == "nodev"
        has_nosuid = has_nosuid || options[option] == "nosuid"
        has_noexec = has_noexec || options[option] == "noexec"
      }
      if (!has_ro || !has_nodev || !has_nosuid || !has_noexec) {
        next
      }
      for (field = 7; field <= NF; field++) {
        if ($field == "-" &&
            $(field + 1) == "tmpfs" &&
            $(field + 2) == "autostream-encoder-recorder-installer-test-sealed-mnt") {
          found = 1
        }
      }
    }
    END { exit found ? 0 : 1 }
  ' /proc/self/mountinfo || die "effective /mnt is not the read-only sealed fixture mount"
  [[ $(stat -f -c '%T' -- /mnt) == "tmpfs" ]] || \
    die "effective /mnt is not backed by the sealed tmpfs"
  [[ $(stat -c '%U:%G:%a' -- /mnt) == "root:root:555" ]] || \
    die "effective /mnt seal has unsafe metadata"
  if touch -- "${probe}" 2>/dev/null; then
    rm -f -- "${probe}"
    die "effective /mnt seal unexpectedly accepted a write"
  fi
}

assert_sealed_scratch_mount
grep -Eq ' /usr .* - overlay autostream-encoder-recorder-installer-test-usr-overlay ' \
  /proc/self/mountinfo || die "isolated /usr overlay mount is missing"
grep -Eq ' /etc .* - overlay autostream-encoder-recorder-installer-test-etc-overlay ' \
  /proc/self/mountinfo || die "isolated /etc overlay mount is missing"
grep -Eq ' /var .* - overlay autostream-encoder-recorder-installer-test-var-overlay ' \
  /proc/self/mountinfo || die "isolated /var overlay mount is missing"
grep -Eq ' /run .* - overlay autostream-encoder-recorder-installer-test-run-overlay ' \
  /proc/self/mountinfo || die "isolated /run overlay mount is missing"
awk '$5 == "/run/systemd" { found=1 } END { exit found ? 0 : 1 }' \
  /proc/self/mountinfo || die "host-backed /run/systemd bind mount is missing"
[[ $(stat -c '%d:%i' -- /run/systemd) == \
  "${AUTOSTREAM_ENCODER_RECORDER_INSTALLER_TEST_RUN_SYSTEMD_ID:-}" ]] || \
  die "host-backed /run/systemd mount does not match its lower source"
awk '$5 == "/run/systemd" && $6 ~ /^rw(,|$)/ { found=1 } END { exit found ? 0 : 1 }' \
  /proc/self/mountinfo || die "host-backed /run/systemd bind mount is not writable"
grep -Eq ' /usr/local/bin .* - tmpfs autostream-encoder-recorder-installer-test-bin ' \
  /proc/self/mountinfo || die "isolated /usr/local/bin mount is missing"
grep -Eq ' /opt .* - tmpfs autostream-encoder-recorder-installer-test-opt ' \
  /proc/self/mountinfo || die "isolated /opt mount is missing"
[[ $(stat -c '%U:%G:%a' -- /mnt) == "root:root:555" ]] || \
  die "could not create an isolated sealed /mnt fixture"
[[ $(stat -c '%U:%G:%a' -- /usr) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr fixture"
[[ $(stat -c '%U:%G:%a' -- /etc) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc fixture"
[[ $(stat -c '%U:%G:%a' -- /etc/systemd) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc/systemd fixture"
[[ $(stat -c '%U:%G:%a' -- /etc/systemd/system) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc/systemd/system fixture"
[[ $(stat -c '%U:%G:%a' -- /usr/local) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local fixture"
[[ $(stat -c '%U:%G:%a' -- /usr/local/bin) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local/bin fixture"
[[ $(stat -c '%U:%G:%a' -- /opt) == "root:root:755" ]] || \
  die "could not create an isolated safe /opt fixture"
[[ $(stat -c '%U:%G:%a' -- /var) == "root:root:755" ]] || \
  die "could not create an isolated safe /var fixture"
[[ $(stat -c '%U:%G:%a' -- /var/lib) == "root:root:755" ]] || \
  die "could not create an isolated safe /var/lib fixture"
[[ $(stat -c '%U:%G:%a' -- /var/backups) == "root:root:755" ]] || \
  die "could not create an isolated safe /var/backups fixture"
[[ $(stat -c '%U:%G:%a' -- /var/tmp) == "root:root:1777" ]] || \
  die "could not create an isolated safe /var/tmp fixture"
[[ $(stat -c '%U:%G:%a' -- /run) == "root:root:755" ]] || \
  die "could not create an isolated safe /run fixture"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
[[ ${SCRIPT_DIR} == /* && -d ${SCRIPT_DIR} ]] || die "could not resolve the fixture directory"
readonly SCRIPT_DIR
readonly INSTALLER_SOURCE="${SCRIPT_DIR}/install-autostream-encoder-recorder"

# Modules share this shell and preserve the original scenario order.
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/fixture-ownership-and-runtime.sh"
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/archive-and-runtime-sentinel-fixture.sh"
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/input-rejection-cases.sh"
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/account-and-lock-rollback-cases.sh"
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/fresh-install-and-legacy-fixture.sh"
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/legacy-rollback-and-recovery-cases.sh"
source "${SCRIPT_DIR}/installer-tests/encoder-recorder/migration-and-runtime-race-cases.sh"
