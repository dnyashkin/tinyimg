#!/usr/bin/env bash
# build.sh — сборка CLI tinyimg под разные платформы в dist/.
#
# Нативная сборка (текущая платформа) — с cgo: полный WebP через libwebp.
# Кросс-сборки — CGO_ENABLED=0: чистый Go, WebP отключён (заглушка
# webp_nocgo.go честно отвечает «webp только в сборке с cgo»); PNG/JPEG
# работают везде одинаково.
#
# Веб-сервис (cmd/tinyimg-web) здесь не собирается: на прод его ставит
# deploy/deploy.sh сборкой на самом сервере (там cgo + libwebp).
#
# Запуск из корня репозитория: ./build.sh [версия]
# Версия по умолчанию — из git (tag или короткий хеш + -dirty).

set -euo pipefail
cd "$(dirname "$0")"

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT=dist
rm -rf "$OUT"
mkdir -p "$OUT"

LDFLAGS="-s -w -X main.version=$VERSION"

# Нативная сборка с cgo (WebP): таргет без суффикса «-nowebp».
HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"
echo "==> native $HOST_OS/$HOST_ARCH (cgo, webp)"
CGO_ENABLED=1 go build -ldflags "$LDFLAGS" \
  -o "$OUT/tinyimg-$HOST_OS-$HOST_ARCH" ./cmd/tinyimg

# Кросс-сборки без cgo (без WebP).
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do
  os="${target%/*}"
  arch="${target#*/}"
  if [[ "$os" == "$HOST_OS" && "$arch" == "$HOST_ARCH" ]]; then
    continue # нативный уже собран с cgo
  fi
  bin="$OUT/tinyimg-$os-$arch"
  [[ "$os" == windows ]] && bin="$bin.exe"
  echo "==> cross $os/$arch (без cgo, webp отключён)"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags "$LDFLAGS" \
    -o "$bin" ./cmd/tinyimg
done

(cd "$OUT" && shasum -a 256 tinyimg-* > SHA256SUMS)
echo "==> готово, $OUT/:"
ls -lh "$OUT" | tail -n +2
