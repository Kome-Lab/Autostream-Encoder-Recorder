# autostream-encoder-recorder

AutoStream の Encoder/Recorder service です。Control Panel から stream job を受け取り、Discord Bot から届く音声、Worker から届く字幕、チャット、participant event、配信枠に紐づくウォーターマーク画像、外部 media input を FFmpeg で最終出力へ合成します。配信中は `final.mkv` を安全に録画し、停止後に `final.mp4` へ remux して Google Drive API へ upload します。

## 責務

- Control Panel への service registration / heartbeat
- stream job の start / stop / retry-upload
- Discord Opus audio ingest bridge
- Worker event の sidecar 保存
- FFmpeg の同一 encode による live output、MKV 録画、HLS preview
- `final.mkv -> final.mp4` packaging
- Google Drive OAuth destination への upload
- Control Panel からの local archive artifact download / rename / delete
- Observability への metric / event / failure signal 送信

## Control Panel 管理

本番運用では Node のID、Control Panel接続先、Node Runtime Token、stream ingest署名鍵、YouTube output、Google Drive destination、Archive profile、OAuth connected account、runtime secretをControl Panelで管理します。Node登録後に表示されるAuto Configureコマンドを対象hostで一度実行すると、Node固有値は`config.yml`へ保存されます。tokenをenvへ転記する必要はありません。

Nodeを作る前に、Control Panel envの`AUTOSTREAM_SECRET_ENCRYPTION_KEY`と`AUTOSTREAM_STREAM_INGEST_SIGNING_KEY`へ、それぞれ32 byte以上のrandom値を設定してください。短い値やplaceholderのままではNode作成・再設定を拒否します。

```text
AUTOSTREAM_NODE_CONFIG=/etc/autostream-encoder-recorder/config.yml
AUTOSTREAM_ENV=production
AUTOSTREAM_BIND_ADDR=127.0.0.1:8081
AUTOSTREAM_CONFIG_REVISION=1
AUTOSTREAM_OUTPUT_RELAY_URL=
AUTOSTREAM_OUTPUT_RELAY_MODE=direct
# Set a Relay URL and an explicit Relay mode only when a Relay is intended.
```

`AUTOSTREAM_CONFIG_REVISION` is a root-owned positive integer used by the local
executor to bind `/updater/version` to the applied service configuration.
It defaults to `1`; increment it after a configuration change. Invalid, signed,
fractional, padded, zero, or negative values stop Encoder/Recorder before it
starts serving HTTP.

The Control Panel local executor writes managed bind/revision overrides to
`/opt/autostream/local-executor/ports/encoder-recorder.env`. systemd loads this optional
root-owned sidecar after `encoder-recorder.env`, so managed values win without
breaking existing hosts where the sidecar does not exist.

systemd 版の待受ポートは `/etc/autostream/encoder-recorder.env` の
`AUTOSTREAM_BIND_ADDR` で変更できます。ポートは非特権範囲の
`1024`～`65535` を指定してください。標準の env ファイルは IPv4
loopback の `127.0.0.1:8081` を明示します。変数自体がない既存環境では、
アップグレードだけでポートを移動しないようバイナリの従来値
`127.0.0.1:8080` を維持します。
例えば `127.0.0.1:18081` に変更した場合、`/health` と
`/updater/version` も同じ `18081` で待ち受けます。不正な形式、範囲外、
または特権ポートを指定した場合は Encoder/Recorder が起動時に停止します。
IPv6 loopback を明示的に使う場合は `[::1]:18081` のように角括弧を含めて
指定し、プローブURLも `http://[::1]:18081/...` とします。

Docker 版ではホスト公開ポートを `AUTOSTREAM_ENCODER_RECORDER_PORT`、
コンテナ内の待受ポートを `AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT`
で個別に変更できます。既定値はそれぞれ `8081` と `8080` で、どちらも
`1024`～`65535` を使用してください。

Compose published ports are a host/reverse-proxy responsibility. The Control
Panel Updater manages only host ports `1024` through `65535`; manually
publishing a privileged or conflicting Docker host port is outside the managed
update contract.

The production health authority is the host Local Executor. These Compose files
intentionally omit an in-container `healthcheck`: the runtime image has no
purpose-built HTTP probe client, and the image contract does not add or repurpose `curl`, `wget`, or another unrelated executable solely for container health.
For managed Docker changes, the Local Executor probes the loopback published port for both `/health` and `/updater/version`; the published port is the health port.
A recreate is accepted only when health, service identity, version, and
`AUTOSTREAM_CONFIG_REVISION` match; otherwise the executor rolls back or reports
`rollback_failed`.

```powershell
$env:AUTOSTREAM_ENCODER_RECORDER_PORT = "18081"
$env:AUTOSTREAM_ENCODER_RECORDER_CONTAINER_PORT = "18080"
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

`AUTOSTREAM_ARCHIVE_DIR=/var/lib/autostream/archives`、`FFMPEG_BIN=ffmpeg`、`TZ`は必要なhostだけで上書きします。上記はコード既定値と重複するため標準envには不要です。

`AUTOSTREAM_ENV=production`でも、通常はControl Panel runtime configだけで直接YouTube Live APIへ送信します。固定Relayを使う場合だけRelay URL、Relay mode、`AUTOSTREAM_REQUIRE_OUTPUT_RELAY=true`を明示してください。通常の直接送信ではRelay URLを空にし、`AUTOSTREAM_OUTPUT_RELAY_MODE=direct`を設定します。signed ingest tokenは環境に関係なく既定で必須です。YouTube stream key、RTMPS URL、Drive folder IDなどのoperational secretはenv fallbackから読まず、Control Panelから受け取ったstream runtime configだけを使います。

`AUTOSTREAM_NODE_CONFIG`が未作成の間はNode Agentがpendingとして待機します。Auto Configure後に`config.yml`が不正、service registrationが失敗、またはruntime config fetchが失敗した場合はfail closedします。

Production mode では `/streams/start` と `/streams/dry-run` の request body に raw `stream_key` を含めると `raw_youtube_stream_key_not_allowed` で拒否します。Control Panel からは `stream_key_secret_name` と runtime secret resolve を使い、Encoder/Recorder の env や API response に YouTube stream key を残しません。

runtime secret reference の解決には service token の `service.secret.resolve` scope が必要です。`service.config.read` だけの token は runtime config を読めますが、YouTube stream key、Drive folder ID、OAuth refresh token などの raw value は取得できません。

Control Panel runtime config が必須の環境で `rtmp_url` / `stream_key` / archive config が不足している start / package request は `youtube_runtime_config_required` として拒否します。
`archive_config.auth_mode` が stream job に含まれる場合も、Google Drive / OAuth の不足値を env から補完しません。

Control Panel runtime config includes `stream_archive_configs` for Drive destination and archive profile binding. Encoder/Recorder applies the ready primary entry for `/streams/start`, `/streams/dry-run`, and `/streams/package`; it copies only non-secret fields and secret reference names such as `folder_id_secret_name`, `client_secret_secret_name`, and `refresh_token_secret_name`. Raw Drive folder IDs, OAuth client secrets, and refresh tokens must be resolved through `/services/runtime-secrets/resolve` and must not be sent in request bodies, env fallback, logs, metadata, or API responses. Service Account authentication is not supported.
The non-secret `retention_days` archive config controls local final archive cleanup. After package completes, Encoder/Recorder removes expired `AUTOSTREAM_ARCHIVE_DIR/final/<stream_id>/` directories for other safe stream IDs and never follows symlinks or deletes outside the archive root.

## Production Output Relay

`AUTOSTREAM_OUTPUT_RELAY_MODE` has exactly three non-secret values:
`direct`, `legacy_stream_key`, and `live_api_static`. With no Relay URL and
When Relay output is not explicitly required, the effective mode is `direct`.
When Relay output is explicitly required (`AUTOSTREAM_REQUIRE_OUTPUT_RELAY=true`),
a missing URL is
unavailable/fail-closed and no output Relay capability is advertised. With a
Relay URL and no mode, Encoder/Recorder
preserves the existing `legacy_stream_key` route: it accepts only a Control
Panel `stream_key` output, clears its upstream URL/key/reference before input
resolution or FFmpeg, and sends FFmpeg only to the local Relay target. It
rejects `live_api`, `live_api_dry_run`, and an absent output mode.

`live_api_static` is an explicit migration only. It requires a Relay URL and
the non-secret `AUTOSTREAM_OUTPUT_RELAY_BINDING_ID`, which must exactly match
the Control Panel YouTube Output profile's `relay_binding_id`. The profile must
be `live_api_relay_static`, ready, and binding-fenced before start or dry-run.
The binding format is exactly `relay-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
with lowercase hexadecimal UUID characters. The binding ID is an identity fence
only; never put the Relay RTMPS URL,
YouTube stream key, or watch URL in it. A Relay URL with `direct`, an unknown
mode, or a URL-free non-direct mode is an invalid configuration. Production
Compose defaults to `legacy_stream_key` for upgrade compatibility. To select
`live_api_static`, set both the mode and binding in `.env`; Encoder/Recorder
preflight rejects the configuration until the binding is supplied. A URL-only
native/systemd configuration remains legacy compatible.

本番では FFmpeg argv に YouTube stream key と upstream RTMPS URL を出しません。host配置ではFFmpegをloopback relayへ、Docker配置では通常のCompose network上の`output-relay:1935`へ出力します。

```text
AUTOSTREAM_OUTPUT_RELAY_URL=rtmp://127.0.0.1/autostream/{stream_id}
```

Docker production composeではEncoder/Recorderと`output-relay`を通常のCompose networkへ接続し、`AUTOSTREAM_OUTPUT_RELAY_URL=rtmp://output-relay:1935/autostream/{stream_id}`でservice DNSを使います。`docker-compose.prod.yml`は同時に`AUTOSTREAM_COMPOSE_OUTPUT_RELAY=1`をcontainer内へ設定し、この固定の`output-relay:1935`だけをrelayとして許可します。この設定は任意hostの許可ではないため、host/systemd配置へコピーしないでください。network namespaceは共有しません。`relay/nginx-rtmp.conf.example`を`relay/nginx-rtmp.conf`にコピーし、YouTube stream keyを置き換えてください。`relay/nginx-rtmp.conf`は`.gitignore`済みです。

Dockerでは先に`config` directoryを作成してcontainer userが書き込めるようにし、PanelのAuto Configure commandと同じ引数をone-shot containerで実行します。host向けの`sudo autostream-encoder-recorder`とDocker one-shotは代替手段であり、両方は実行しません。

```bash
cp .env.example .env
mkdir -p config
sudo chown 65532:65532 config
docker compose -f docker-compose.yml run --rm --no-deps encoder-recorder configure --panel-url "https://control.example.com" --token "<CONFIGURE_TOKEN>" --node "encoder-01" --config "/etc/autostream-encoder-recorder/config.yml"
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d encoder-recorder output-relay
```

one-shot configureではbase composeの書き込み可能mountを使い、生成後のproduction起動では`docker-compose.prod.yml`のread-only mountを使います。

host 配置では、同一 host の nginx-rtmp、SRS、または同等の relay を loopback だけで待ち受けさせてください。relay 設定ファイルは Git 管理外に置き、owner/read permission を限定します。

## 互換 fallback

local/dev または移行期間だけ、次の env fallback を使えます。本番標準ではありません。

```text
YOUTUBE_RTMP_URL=rtmps://a.rtmps.youtube.com/live2
YOUTUBE_STREAM_KEY=<YOUTUBE_STREAM_KEY>
```

Google Drive archive upload には env fallback を使いません。Control Panel の配信枠設定で OAuth account、folder ID、shared drive ID、archive file name を指定します。共有ドライブの folder ID を使う場合は stream archive settings で shared drive を有効化します。Uploader は Drive API に `supportsAllDrives=true` を付けて folder / file 操作を行います。

Encoder/Recorder は Control Panel へ報告した final artifact を `AUTOSTREAM_ARCHIVE_DIR/final/<stream_id>/` 配下に一定期間保持します。Control Panel は割り当て済みの primary encoder に service token 付きで接続し、録画ファイルの download、rename、delete を行います。対象ファイル名は安全な basename と `.mp4`、`.mkv`、`.json`、`.jsonl`、`.vtt` に限定し、symlink と archive root 外への移動は拒否します。

## HLS Preview

live FFmpeg process は RTMP と `final.mkv` に使う同一 encode packet を tee し、`AUTOSTREAM_ARCHIVE_DIR/tmp/<stream_id>/preview/` に約 2 秒単位、最大 6 segment の rolling HLS preview を生成します。preview slave だけを有限 FIFO queue に分離し、`onfail=ignore` と queue overflow 時の packet drop を設定するため、preview の stall/open/write 障害では RTMP と録画を停止しません。正常終了時は playlist に `#EXT-X-ENDLIST` を書きます。

Control Panel は service token を付けて `GET /streams/{id}/preview/index.m3u8` と、playlist 内の相対 URL が指す `GET /streams/{id}/preview/segment-NNNNNN.ts` を proxy できます。playlist は `Cache-Control: no-store`、segment は `Cache-Control: private, max-age=30` です。segment endpoint は byte range に対応するため、Control Panel の proxy でも `Authorization` と `Range` を転送し、playlist と segment の同じ相対 path 構造を維持してください。VLC などの player にはこの Control Panel proxy URL を渡します。

preview endpoint は `Authorization: Bearer <service-token>` を必須とし、上記 2 種類以外の名前、path traversal、symlink、非 regular file を拒否します。process status と start metadata にはローカル絶対 path を返さず、`preview/index.m3u8` だけを論理名として公開します。

## Input Policy

HLS direct input は既定で無効です。必要な場合だけ明示的に opt-in し、入力 URL の allowlist と DNS 解決後の private network 拒否を維持してください。

## 開発

```powershell
go test ./...
go build ./...
```

docs / deployment sample を更新した場合:

```powershell
cd ..\autostream-docs
npm run docs:check
npm run docs:build
```

Detailed deployment, archive, and security documentation is maintained in the `autostream-docs` repository.

## セキュリティ

- stream key、OAuth refresh token、Drive credential、webhook URL は raw でログに出しません。
- Google Drive config を表示する場合も configured status と fingerprint だけを使います。
- FFmpeg は shell string ではなく argument array で起動します。
- HLS preview の playlist と segment は stream 固有の `tmp` directory に限定し、open 前後に regular file と symlink の状態を再検証します。
- archive path は `AUTOSTREAM_ARCHIVE_DIR` 配下に制限し、Control Panel からの録画ファイル操作も `final/<stream_id>/<file>` だけを対象にします。
- Configure TokenはControl Panel側でhash保存し、Node Runtime Tokenは暗号化保存します。どちらも通常画面では再表示しません。
- runtime config は service assignment と service type を検証してから取得します。
