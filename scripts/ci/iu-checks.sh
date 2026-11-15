#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly GOTELEMETRY=off
: "${RUNNER_TEMP:?IU checks require an explicit disposable output directory}"
baseline=8c17e7389ccdb3429f1e59a1a6147b27fe3c3793
test "$(go env GOVERSION)" = go1.26.5
python3 -B -m unittest discover -s scripts/ci -p 'test_iu_*.py' -v
python3 scripts/ci/iu-source-boundary.py
python3 scripts/ci/iu-module-pin.py "$baseline" --download
python3 scripts/ci/iu-secret-scan.py
mapfile -t files < <(git diff --name-only --diff-filter=AM "$baseline" HEAD -- '*.go')
test "${#files[@]}" -gt 0
test -z "$(gofmt -l "${files[@]}")"
packages=(./internal/lifecycleparticipant ./internal/audioingest ./internal/httpapi)
for mode in native cgo-free race; do
  flags=()
  if test "$mode" = race; then flags=(-race); fi
  if test "$mode" = cgo-free; then export CGO_ENABLED=0; else export CGO_ENABLED=1; fi
  go test -p 1 -count=1 -timeout=10m "${flags[@]}" -json "${packages[@]}" -run '^TestIUWS3' | tee "$RUNNER_TEMP/iu-$mode.json"
  python3 scripts/ci/iu-json-events.py scripts/ci/iu-required-tests.txt "$RUNNER_TEMP/iu-$mode.json"
done
go vet -p 1 ./...
mkdir -p "$RUNNER_TEMP/iu-cross"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -p 1 -trimpath "${packages[@]}"
  for package in "${packages[@]}"; do
    output="$RUNNER_TEMP/iu-cross/${package##*/}-linux-$arch.test"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -p 1 -trimpath -c "$package" -o "$output"
    go version -m "$output"
    sha256sum "$output"
  done
done
# Cross-built binaries are not executed. Native JSON above proves execution;
# existing go job still runs all package tests/builds and actual FFmpeg fixtures.
git diff --exit-code -- go.mod go.sum
python3 scripts/ci/iu-source-boundary.py
