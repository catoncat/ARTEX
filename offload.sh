#!/bin/sh
# ARTEX 空间卸载：把 /app/data 下的老归档与老证据 blobs 推到 Google Drive，
# 推成功后删本地，腾磁盘。rclone 靠环境变量配置（RCLONE_CONFIG_GDRIVE_*）；
# 没配令牌时直接跳过，不打扰主流程。
set -u

REMOTE="${ARTEX_OFFLOAD_REMOTE:-gdrive:artex-offload}"
DATA_DIR="${ARTEX_DATA_DIR:-/app/data}"
ARCHIVE_MIN_AGE="${ARTEX_OFFLOAD_ARCHIVE_MIN_AGE:-24h}"
BLOB_MIN_AGE="${ARTEX_OFFLOAD_BLOB_MIN_AGE:-7d}"

if [ -z "${RCLONE_CONFIG_GDRIVE_TOKEN:-}" ]; then
  echo "[offload] 未配置 Drive 令牌，跳过"
  exit 0
fi

# 光有 token 建不起远端：env 方式配置 rclone 还必须有 type（scope 默认 drive 全量）。
# Railway 侧只注入 token 时靠这里补默认值；外部已显式设置则不覆盖。
export RCLONE_CONFIG_GDRIVE_TYPE="${RCLONE_CONFIG_GDRIVE_TYPE:-drive}"
export RCLONE_CONFIG_GDRIVE_SCOPE="${RCLONE_CONFIG_GDRIVE_SCOPE:-drive}"

echo "[offload] 开始同步 -> $REMOTE"
if [ -d "$DATA_DIR/archives" ]; then
  rclone move "$DATA_DIR/archives" "$REMOTE/archives" \
    --min-age "$ARCHIVE_MIN_AGE" --delete-empty-src-dirs \
    --transfers 2 --checkers 4 --retries 2 \
    --contimeout 30s --timeout 120s --log-level NOTICE || true
fi
if [ -d "$DATA_DIR/blobs" ]; then
  rclone move "$DATA_DIR/blobs" "$REMOTE/blobs" \
    --min-age "$BLOB_MIN_AGE" --delete-empty-src-dirs \
    --transfers 2 --checkers 4 --retries 2 \
    --contimeout 30s --timeout 120s --log-level NOTICE || true
fi
# --delete-empty-src-dirs 可能把 archives/、blobs/ 目录本身删掉，重建空目录，
# 免得主程序往不存在的目录写文件。
mkdir -p "$DATA_DIR/archives" "$DATA_DIR/blobs" 2>/dev/null || true
echo "[offload] 本轮完成"
