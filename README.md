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

本番運用では YouTube output、Google Drive destination、Archive profile、OAuth connected account、runtime secret は Control Panel で管理します。env は service が Control Panel へ接続するための bootstrap に限定します。

```text
SERVICE_ID=encoder-main-1
SERVICE_NAME=Encoder Main 1
SERVICE_PUBLIC_URL=https://encoder-1.example.com
CONTROL_PANEL_URL=https://control.example.com
CONTROL_PANEL_TOKEN=<SERVICE_TOKEN>
SERVICE_CONTROL_TOKEN_SHA256=<SHA256_FOR_INBOUND_CONTROL>
AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true
AUTOSTREAM_ENV=production
AUTOSTREAM_REQUIRE_OUTPUT_RELAY=true
AUTOSTREAM_OUTPUT_RELAY_URL=rtmp://127.0.0.1/autostream/{stream_id}
AUTOSTREAM_DATA_DIR=/var/lib/autostream/encoder-recorder
AUTOSTREAM_ARCHIVE_DIR=/var/lib/autostream/archives
FFMPEG_BIN=ffmpeg
TZ=Asia/Tokyo
```

`AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true` の場合、YouTube stream key、RTMPS URL、Drive folder ID などの operational secret は env fallback から読みません。Control Panel から受け取った stream runtime config だけを使います。

`AUTOSTREAM_ENV=production` または `AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true` の起動では、`CONTROL_PANEL_URL` と `CONTROL_PANEL_TOKEN` が未設定、service registration が失敗、または runtime config fetch が失敗した場合に process を fail closed します。handler で request を拒否するだけでなく、事前登録、heartbeat、runtime config の Control Panel 境界が成立しない service を production ready として起動しません。

Production mode では `/streams/start` と `/streams/dry-run` の request body に raw `stream_key` を含めると `raw_youtube_stream_key_not_allowed` で拒否します。Control Panel からは `stream_key_secret_name` と runtime secret resolve を使い、Encoder/Recorder の env や API response に YouTube stream key を残しません。

runtime secret reference の解決には service token の `service.secret.resolve` scope が必要です。`service.config.read` だけの token は runtime config を読めますが、YouTube stream key、Drive folder ID、OAuth refresh token などの raw value は取得できません。

Control Panel runtime config が必須の環境で `rtmp_url` / `stream_key` / archive config が不足している start / package request は `youtube_runtime_config_required` として拒否します。
`archive_config.auth_mode` が stream job に含まれる場合も、Google Drive / OAuth の不足値を env から補完しません。

Control Panel runtime config includes `stream_archive_configs` for Drive destination and archive profile binding. Encoder/Recorder applies the ready primary entry for `/streams/start`, `/streams/dry-run`, and `/streams/package`; it copies only non-secret fields and secret reference names such as `folder_id_secret_name`, `client_secret_secret_name`, and `refresh_token_secret_name`. Raw Drive folder IDs, OAuth client secrets, and refresh tokens must be resolved through `/services/runtime-secrets/resolve` and must not be sent in request bodies, env fallback, logs, metadata, or API responses. Service Account authentication is not supported.
The non-secret `retention_days` archive config controls local final archive cleanup. After package completes, Encoder/Recorder removes expired `AUTOSTREAM_ARCHIVE_DIR/final/<stream_id>/` directories for other safe stream IDs and never follows symlinks or deletes outside the archive root.

## Production Output Relay

本番では FFmpeg argv に YouTube stream key と upstream RTMPS URL を出しません。FFmpeg は loopback relay にだけ出力します。

```text
AUTOSTREAM_OUTPUT_RELAY_URL=rtmp://127.0.0.1/autostream/{stream_id}
```

Docker production compose では `output-relay` sidecar を Encoder/Recorder と同じ network namespace で起動します。`relay/nginx-rtmp.conf.example` を `relay/nginx-rtmp.conf` にコピーし、YouTube stream key を置き換えてください。`relay/nginx-rtmp.conf` は `.gitignore` 済みです。

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
- service token は Control Panel 側で hash 保存し、再表示しません。
- runtime config は service assignment と service type を検証してから取得します。
