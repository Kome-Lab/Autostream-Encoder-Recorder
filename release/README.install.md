# AutoStream Encoder Recorder Host Install

This archive contains the Linux binary, systemd example, and placeholder environment file for the AutoStream Encoder Recorder.

## Requirements

- Linux amd64 or arm64 matching the archive name, with systemd and `sudo`.
- `jq`, `sha256sum`, `flock`, and GNU `tar`.
- `ffmpeg` installed on the host and available on `PATH`.
- `gh` installed and authenticated on the administrator workstation used to
  verify the release before upload. The server does not need `gh`, a GitHub
  token, or GitHub network access for manual installation.
- Network access to the Control Panel, output relay, and configured providers.
- A Control Panel configured with independent random `AUTOSTREAM_SECRET_ENCRYPTION_KEY` and `AUTOSTREAM_STREAM_INGEST_SIGNING_KEY` values of at least 32 bytes; placeholders are rejected by Node operations.

## Install a verified managed release

Download only
`autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz` (`arm64` on an arm64
host). On the administrator workstation, verify that exact archive before
uploading it:

```bash
gh attestation verify autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz --repo Kome-Lab/Autostream-Encoder-Recorder --signer-workflow Kome-Lab/Autostream-Encoder-Recorder/.github/workflows/release-host.yml --deny-self-hosted-runners
scp autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz operator@encoder.example.com:/tmp/
```

On the server, copy the one uploaded archive into a root-owned staging
directory, extract it there without renaming its top-level directory, leave the
original `.tar.gz` beside that directory until the installer finishes, and run
the installer:

```bash
sudo install -d -o root -g root -m 0755 /opt/autostream/releases
sudo install -d -o root -g root -m 0755 /opt/autostream/releases/artifacts
sudo install -o root -g root -m 0644 /tmp/autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz /opt/autostream/releases/artifacts/autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz
cd /opt/autostream/releases/artifacts
sudo test ! -e autostream-encoder-recorder_vX.Y.Z_linux_amd64
sudo test ! -L autostream-encoder-recorder_vX.Y.Z_linux_amd64
sudo tar --no-same-owner --no-same-permissions -xzf autostream-encoder-recorder_vX.Y.Z_linux_amd64.tar.gz
cd autostream-encoder-recorder_vX.Y.Z_linux_amd64
sudo ./install-autostream-encoder-recorder
```

Manual installation does not require the archive `.sha256`,
`release-manifest.json`, or `release-manifest.json.sha256`. Those assets remain
published for managed-updater compatibility and are ignored by this installer
even when they are present.

For arm64, replace `amd64` with `arm64` in the archive and directory names.

The installer makes a stable private copy of the adjacent archive, calculates
and records its SHA-256, then verifies the archive layout, inner checksums,
exact `artifact-manifest.json` tuple, architecture, and binary
version/commit/build date before changing persistent host state. It creates the
`autostream` service account when absent, seeds the verified rollback release,
preserves an existing environment file byte for byte, installs the systemd
unit, and exposes
`/usr/local/bin/autostream-encoder-recorder` plus the `encoder-recorder` alias.
It requires `ffmpeg` to already be installed and does not install packages,
write Node configuration, or start the service.
Legacy public binaries are backed up only under the root-owned
`/var/backups/autostream/install-migrations/encoder-recorder` tree.

`/opt/autostream/encoder-recorder/releases` and its `current` link are
installer-owned implementation details used by managed update and rollback.
Do not create or edit their release directories or marker files manually.

Review the non-secret host settings in `/etc/autostream/encoder-recorder.env`.
`AUTOSTREAM_BIND_ADDR` accepts an arbitrary unprivileged port from `1024`
through `65535`; the shipped systemd env uses the standard IPv4 loopback value
`127.0.0.1:8081`. The binary retains the legacy `127.0.0.1:8080` fallback only
when the variable is absent, so upgrading an older installation does not move
its port. The systemd unit does not hard-code a port. An invalid address or an
out-of-range port makes the service fail closed during startup. Keep
`AUTOSTREAM_CONFIG_REVISION=1` for an
existing installation until the Control Panel applies a newer service
configuration. The value is reported by `/updater/version` and must be an
integer greater than or equal to `1`; an invalid value stops the service before
it starts listening. Keep this environment file root-owned and mode `0640`.
Do not add `SERVICE_ID`,
`CONTROL_PANEL_TOKEN`, `SERVICE_CONTROL_TOKEN`, Worker event tokens, or Discord
audio tokens.

In Control Panel, create an `encoder_recorder` Node and run its one-time Auto Configure command on this host. The command writes the Node ID, Control Panel URL, Node Runtime Token, and stream-ingest signing key to `/etc/autostream-encoder-recorder/config.yml` with restricted permissions. For example, use the exact command generated by your Panel:

```bash
sudo autostream-encoder-recorder configure --panel-url "https://control.example.com" --token "<CONFIGURE_TOKEN>" --node "encoder-01" --config "/etc/autostream-encoder-recorder/config.yml"
```

After saving the environment file and completing Auto Configure, start the
service:

```bash
sudo systemctl enable --now autostream-encoder-recorder
sudo systemctl status autostream-encoder-recorder
autostream-encoder-recorder --version
```

When migrating an installation that is already active, restart it after the
installer finishes instead of using the first-start command:

```bash
sudo systemctl restart autostream-encoder-recorder
sudo systemctl status autostream-encoder-recorder
autostream-encoder-recorder --version
```

Check `/health` and `/updater/version` on the host and port configured in
`AUTOSTREAM_BIND_ADDR`. This guide intentionally avoids the older
variable-heavy probe forms `PROBE_HOST="${PROBE_HOST:-127.0.0.1}"` and
`PROBE_HOST='[::1]'`; for IPv6, keep the address in brackets in the URL.

`/updater/version` is the unauthenticated, minimal endpoint used only to prove
the running binary and local service identity to the update helper. Its exact
response fields are version, service_id, service_type, and config_revision.
Block this exact path at any public reverse proxy.

Do not fabricate `.artifact-sha256`, `.version`, `artifact-manifest.json`, or
`checksums.txt` from an unverified local binary. A manual-install archive
without its verified internal manifest is rejected; publish a new release
instead of modifying an existing release asset.

Do not commit real `.env` files, provider credentials, tokens, logs, screenshots, or verification record.
