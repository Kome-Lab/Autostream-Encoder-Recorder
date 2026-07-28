# AutoStream Encoder Recorder Host Install

This archive contains the Linux binary, systemd example, and placeholder environment file for the AutoStream Encoder Recorder.

## Requirements

- Linux amd64 or arm64 matching the archive name.
- A dedicated `autostream` user and group.
- Authenticated `gh`, `jq`, `sha256sum`, and `curl` for release verification.
- `ffmpeg` installed on the host and available on `PATH`.
- Writable archive and runtime directories.
- Network access to the Control Panel, output relay, and configured providers.
- A Control Panel configured with independent random `AUTOSTREAM_SECRET_ENCRYPTION_KEY` and `AUTOSTREAM_STREAM_INGEST_SIGNING_KEY` values of at least 32 bytes; placeholders are rejected by Node operations.

## Install a verified managed release

The systemd unit runs the binary through
`/opt/autostream/encoder-recorder/current`. Seed that link from the same
immutable release manifest and checksum that supplied the archive.
`autostream-updater` refuses an unseeded target because it would have no
verified rollback release.

```bash
set -euo pipefail
VERSION="${VERSION:?export VERSION=vX.Y.Z before continuing}"
ARCH="${ARCH:-amd64}"
ASSET="autostream-encoder-recorder_${VERSION}_linux_${ARCH}.tar.gz"
ARTIFACT_ROOT=/opt/autostream/releases

sudo apt-get update
sudo apt-get install -y ffmpeg
sudo install -d -o root -g root -m 0755 "$ARTIFACT_ROOT"
sudo install -d -o "$USER" -g "$USER" -m 0755 "$ARTIFACT_ROOT/artifacts"
gh release download "$VERSION" \
  --repo Kome-Lab/Autostream-Encoder-Recorder \
  --pattern "$ASSET" \
  --pattern "$ASSET.sha256" \
  --pattern release-manifest.json \
  --pattern release-manifest.json.sha256 \
  --dir "$ARTIFACT_ROOT/artifacts" \
  --clobber
(cd "$ARTIFACT_ROOT/artifacts" && sha256sum --check --strict "$ASSET.sha256")
(cd "$ARTIFACT_ROOT/artifacts" && sha256sum --check --strict release-manifest.json.sha256)

DIGEST="$(awk 'NR == 1 { print $1 }' "$ARTIFACT_ROOT/artifacts/$ASSET.sha256")"
[[ "$DIGEST" =~ ^[0-9a-f]{64}$ ]]
jq -e --arg version "$VERSION" --arg asset "$ASSET" --arg sha "$DIGEST" \
  '.schema_version == 1 and .release_id == $version and .channel == "host" and
   ([.components[] | select(.service == "encoder-recorder" and .source_version == $version) |
     .artifacts[] | select(.name == $asset and .sha256 == $sha)] | length == 1)' \
  "$ARTIFACT_ROOT/artifacts/release-manifest.json"

RELEASE_ROOT=/opt/autostream/encoder-recorder/releases
RELEASE_DIR="$RELEASE_ROOT/${VERSION}-${DIGEST:0:12}"
CURRENT_LINK=/opt/autostream/encoder-recorder/current
sudo test ! -e "$RELEASE_DIR"
sudo install -d -o root -g root -m 0755 "$RELEASE_DIR"
sudo tar --no-same-owner --strip-components=1 -xzf "$ARTIFACT_ROOT/artifacts/$ASSET" -C "$RELEASE_DIR"
(cd "$RELEASE_DIR" && sha256sum --check --strict checksums.txt)
printf '%s\n' "$DIGEST" | sudo tee "$RELEASE_DIR/.artifact-sha256" >/dev/null
printf '%s\n' "$VERSION" | sudo tee "$RELEASE_DIR/.version" >/dev/null
sudo chown root:root "$RELEASE_DIR/.artifact-sha256" "$RELEASE_DIR/.version"
sudo chmod 0444 "$RELEASE_DIR/.artifact-sha256" "$RELEASE_DIR/.version"
sudo /usr/sbin/runuser -u autostream -- "$RELEASE_DIR/bin/autostream-encoder-recorder" --version | grep -F -- "$VERSION"

sudo ln -s "$RELEASE_DIR" "${CURRENT_LINK}.next"
sudo mv -Tf "${CURRENT_LINK}.next" "$CURRENT_LINK"
sudo ln -sfn "$CURRENT_LINK/bin/autostream-encoder-recorder" /usr/local/bin/autostream-encoder-recorder
sudo ln -sfn /usr/local/bin/autostream-encoder-recorder /usr/local/bin/encoder-recorder
sudo install -d -o autostream -g autostream /var/lib/autostream/encoder-recorder /var/lib/autostream/archives
sudo install -d -o root -g root -m 0750 /etc/autostream
sudo install -o root -g root -m 0644 "$RELEASE_DIR/systemd/autostream-encoder-recorder.service.example" /etc/systemd/system/autostream-encoder-recorder.service
if ! sudo test -e /etc/autostream/encoder-recorder.env; then
  sudo install -o root -g root -m 0640 "$RELEASE_DIR/.env.example" /etc/autostream/encoder-recorder.env
else
  echo "preserving existing /etc/autostream/encoder-recorder.env; review .env.example for new settings"
fi
```

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

Set `ENCODER_RECORDER_PORT` below to the port component of
`AUTOSTREAM_BIND_ADDR`, then start the service:

```bash
set -euo pipefail
VERSION="${VERSION:?export VERSION=vX.Y.Z before continuing}"
ENCODER_RECORDER_PORT="${ENCODER_RECORDER_PORT:-8081}"
PROBE_HOST="${PROBE_HOST:-127.0.0.1}"
[[ "$ENCODER_RECORDER_PORT" =~ ^[0-9]+$ ]]
(( ENCODER_RECORDER_PORT >= 1024 && ENCODER_RECORDER_PORT <= 65535 ))
sudo systemctl daemon-reload
sudo systemctl enable autostream-encoder-recorder
sudo systemctl restart autostream-encoder-recorder
PID="$(sudo systemctl show --property=MainPID --value autostream-encoder-recorder)"
EXPECTED="$(sudo readlink -f /opt/autostream/encoder-recorder/current/bin/autostream-encoder-recorder)"
test "$(sudo readlink -f "/proc/$PID/exe")" = "$EXPECTED"
curl --fail --silent --show-error --max-time 10 \
  "http://${PROBE_HOST}:${ENCODER_RECORDER_PORT}/health" >/dev/null
test "$(curl --fail --silent --show-error --max-time 10 \
  "http://${PROBE_HOST}:${ENCODER_RECORDER_PORT}/updater/version" | jq -r '.version')" = "$VERSION"
```

The standard systemd/local-executor profile uses IPv4 loopback. For an explicit
IPv6 loopback bind such as `AUTOSTREAM_BIND_ADDR=[::1]:18081`, run the smoke
check with `PROBE_HOST='[::1]' ENCODER_RECORDER_PORT=18081`; the brackets are
required in the URL.

`/updater/version` is the unauthenticated, minimal endpoint used only to prove
the running binary and local service identity to the update helper. Its exact
response fields are version, service_id, service_type, and config_revision.
Block this exact path at any public reverse proxy.

Do not fabricate `.artifact-sha256` or `.version` from an unverified local
binary. Releases without `release-manifest.json` remain manual-only; publish a
new release instead of modifying an existing release asset.

Do not commit real `.env` files, provider credentials, tokens, logs, screenshots, or verification record.
