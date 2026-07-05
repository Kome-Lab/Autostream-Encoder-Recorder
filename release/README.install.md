# AutoStream Encoder Recorder Host Install

This archive contains the Linux binary, systemd example, and placeholder environment file for the AutoStream Encoder Recorder.

## Requirements

- Linux amd64 or arm64 matching the archive name.
- A dedicated `autostream` user and group.
- `ffmpeg` installed on the host and available on `PATH`.
- Writable archive and runtime directories.
- Network access to the Control Panel, output relay, and configured providers.

## Install

```bash
sudo apt-get update
sudo apt-get install -y ffmpeg
sudo install -o root -g root -m 0755 bin/autostream-encoder-recorder /usr/local/bin/autostream-encoder-recorder
sudo ln -sf /usr/local/bin/autostream-encoder-recorder /usr/local/bin/encoder-recorder
sudo install -d -o autostream -g autostream /var/lib/autostream/encoder-recorder /var/lib/autostream/archives
sudo install -o root -g root -m 0644 systemd/autostream-encoder-recorder.service.example /etc/systemd/system/autostream-encoder-recorder.service
sudo install -o root -g root -m 0640 .env.example /etc/autostream/encoder-recorder.env
```

Edit `/etc/autostream/encoder-recorder.env` with real environment-specific values, then run:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now autostream-encoder-recorder
```

Do not commit real `.env` files, provider credentials, tokens, logs, screenshots, or verification record.
