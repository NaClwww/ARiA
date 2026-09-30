#!/usr/bin/env bash
# aria-host 启动器：接线在 aria.override.toml [host]（机器文件），
# 秘密在 .env（.gitignore 已覆盖，存在即自动加载）。
# 临时变体直接透传 flag，如 ./start.sh --no-stdin / --fake --no-asr。
set -euo pipefail

cd -- "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"

# .env（可选）：ARIA_API_KEY 之类，set -a 让逐行 KEY=VALUE 全部 export
if [ -f .env ]; then
  set -a
  . ./.env
  set +a
fi

# 音箱 adb 在线是前提（设备重启过要先重新授权）
adb forward tcp:18900 tcp:8900

exec go run ./cmd/aria-host "$@"
