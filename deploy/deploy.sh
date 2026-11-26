#!/usr/bin/env bash
# deploy.sh — выкладка tinyimg на прод: rsync исходников → сборка на сервере
# → перезапуск сервиса → дымовой тест. НИКОГДА не заливать бинарь с macOS:
# сервис падает с "Exec format error" (сборка только на сервере, там cgo+libwebp).
#
# Параметры прода (хост, пути, юнит) в репозитории не фиксируем — задаются
# через переменные окружения или deploy/deploy.env (в .gitignore):
#
#   DEPLOY_TARGET  куда ssh'иться (user@host)            — обязательно
#   DEPLOY_DIR     корень на сервере (src/ и bin/ внутри) — обязательно
#   SERVICE        systemd-юнит          (по умолчанию tinyimg)
#   SERVICE_USER   владелец бинарей      (по умолчанию tinyimg)
#   WEB_PORT       loopback-порт бэкенда (по умолчанию 8099)
#
# Запуск из корня репозитория: ./deploy/deploy.sh

set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -f deploy/deploy.env ]]; then
  # shellcheck disable=SC1091
  source deploy/deploy.env
fi

: "${DEPLOY_TARGET:?задай DEPLOY_TARGET (user@host) — см. шапку скрипта}"
: "${DEPLOY_DIR:?задай DEPLOY_DIR (корень на сервере) — см. шапку скрипта}"
SERVICE="${SERVICE:-tinyimg}"
SERVICE_USER="${SERVICE_USER:-tinyimg}"
WEB_PORT="${WEB_PORT:-8099}"

SSH="ssh -o BatchMode=yes $DEPLOY_TARGET"

echo "==> 1/4 rsync исходников -> $DEPLOY_TARGET:$DEPLOY_DIR/src/"
rsync -az --delete \
  --exclude=.git \
  --exclude=research/orginal \
  --exclude=research/compressed_by_tinypng \
  --exclude=research/compare \
  --exclude=research/tool \
  --exclude=output \
  ./ "$DEPLOY_TARGET:$DEPLOY_DIR/src/"

echo "==> 2/4 сборка на сервере (cgo + libwebp)"
$SSH "export PATH=/usr/local/go/bin:\$PATH && cd $DEPLOY_DIR/src \
  && CGO_ENABLED=1 go build -o $DEPLOY_DIR/bin/tinyimg-web ./cmd/tinyimg-web \
  && CGO_ENABLED=1 go build -o $DEPLOY_DIR/bin/tinyimg ./cmd/tinyimg \
  && chown $SERVICE_USER:$SERVICE_USER $DEPLOY_DIR/bin/tinyimg $DEPLOY_DIR/bin/tinyimg-web \
  && chmod 755 $DEPLOY_DIR/bin/tinyimg $DEPLOY_DIR/bin/tinyimg-web"

echo "==> 3/4 перезапуск сервиса $SERVICE"
$SSH "systemctl restart $SERVICE && sleep 2 && systemctl is-active $SERVICE"

echo "==> 4/4 дымовой тест (POST /api/compress на loopback)"
# Тестовая картинка встроена в скрипт (48x48, градиент+шум — проходит
# полную палитровую ветку PNG-пайплайна), чтобы не зависеть от файлов репо.
SMOKE_IMG="$(mktemp /tmp/tinyimg_smoke.XXXXXX.png)"
trap 'rm -f "$SMOKE_IMG"' EXIT
base64 -d >"$SMOKE_IMG" <<'SMOKE_B64'
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
SMOKE_B64
scp -q "$SMOKE_IMG" "$DEPLOY_TARGET:/tmp/tinyimg_smoke.png"
RESP=$($SSH "curl -s -X POST -F 'file=@/tmp/tinyimg_smoke.png' http://127.0.0.1:$WEB_PORT/api/compress; rm -f /tmp/tinyimg_smoke.png")
echo "$RESP" | grep -q '"ok":true' || { echo "дым-тест провален: $RESP" >&2; exit 1; }
echo "дым-тест ок: $RESP"

echo "==> готово: $DEPLOY_TARGET, сервис $SERVICE активен"
