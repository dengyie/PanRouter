#!/bin/bash
# 每日 SQLite VACUUM INTO 到 NFS。/data 是 Bohrium overlay,容器重建即丢。
set -u
BK=/personal/pxed/panrouter/backups
DB=/data/panrouter/data/panrouter.db
mkdir -p "$BK" /data/panrouter/logs
while :; do
  if [ -f "$DB" ]; then
    TS=$(date +%Y%m%d-%H%M%S)
    sqlite3 "$DB" "VACUUM INTO '$BK/panrouter-$TS.db'" 2>>/data/panrouter/logs/backup.err || true
    ls -1t "$BK"/panrouter-*.db 2>/dev/null | tail -n +15 | xargs -r rm -f
  fi
  sleep 86400
done
