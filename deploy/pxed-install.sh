#!/usr/bin/env bash
# 远端原子安装:由 GitHub Actions 交叉编译 linux/amd64 后 scp + 调用。
# 不写密钥、不覆盖 /personal/pxed/panrouter/config.yaml。
# 控制面(config/env/run.sh/supervisor)由首次 bootstrap 落在 NFS,本脚本只换二进制并拉活。
set -euo pipefail

SRC="${1:?binary path}"
BIN_DIR=/data/panrouter/bin
CTL=/personal/pxed
SUP_CONF=/personal/pxed/supervisord.conf
PROG_CONF=/personal/pxed/supervisor-panrouter.conf

mkdir -p "$BIN_DIR" /data/panrouter/data /data/panrouter/logs \
  /personal/pxed/panrouter/backups

if [[ ! -f "$SRC" ]]; then
  echo "missing binary: $SRC" >&2
  exit 1
fi
python3 - "$SRC" <<'PY'
import struct, sys
p = sys.argv[1]
with open(p, "rb") as f:
    if f.read(4) != b"\x7fELF":
        sys.exit("refusing non-ELF binary: " + p)
    ei_class = f.read(1)[0]
    f.seek(18)
    e_machine = struct.unpack("<H", f.read(2))[0]
if ei_class != 2 or e_machine != 62:
    sys.exit(f"want ELF64 x86_64, got class={ei_class} machine={e_machine}")
PY

install -m 755 "$SRC" "$BIN_DIR/panrouter.new"
mv -f "$BIN_DIR/panrouter.new" "$BIN_DIR/panrouter"
date -u +%Y-%m-%dT%H:%M:%SZ > /data/panrouter/bin/DEPLOY_STAMP

if [[ ! -x /personal/pxed/panrouter/run.sh ]]; then
  echo "control plane missing: /personal/pxed/panrouter/run.sh (bootstrap first)" >&2
  exit 1
fi
if [[ ! -f /personal/pxed/panrouter/config.yaml ]]; then
  echo "config missing: /personal/pxed/panrouter/config.yaml (bootstrap first)" >&2
  exit 1
fi

if [[ ! -f "$PROG_CONF" ]]; then
  cat > "$PROG_CONF" <<'EOF'
[program:panrouter]
command=/bin/bash /personal/pxed/panrouter/run.sh
autostart=true
autorestart=true
startsecs=2
startretries=8
stopwaitsecs=20
stopasgroup=true
killasgroup=true
stdout_logfile=/data/panrouter/logs/out.log
stdout_logfile_maxbytes=10MB
stdout_logfile_backups=3
stderr_logfile=/data/panrouter/logs/err.log
stderr_logfile_maxbytes=10MB
stderr_logfile_backups=3

[program:panrouter-backup]
command=/bin/bash /personal/pxed/panrouter/backup.sh
autostart=true
autorestart=true
startsecs=5
stdout_logfile=/data/panrouter/logs/backup.log
stdout_logfile_maxbytes=5MB
stdout_logfile_backups=2
stderr_logfile=/data/panrouter/logs/backup.err
stderr_logfile_maxbytes=5MB
stderr_logfile_backups=2
EOF
fi

if [[ -f "$SUP_CONF" ]] && ! grep -q 'supervisor-panrouter.conf' "$SUP_CONF"; then
  python3 - "$SUP_CONF" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
text = p.read_text()
needle = "/personal/pxed/supervisor-panrouter.conf"
if needle in text:
    raise SystemExit(0)
# 追加到 [include] files = 行末;catch 不到则在文件末加一行。
lines = text.splitlines(True)
out, done = [], False
for line in lines:
    if (not done) and line.lstrip().startswith("files") and "=" in line:
        stripped = line.rstrip("\n")
        pad = "\n" if line.endswith("\n") else ""
        if stripped.endswith("\\"):
            out.append(line)
            continue
        out.append(stripped + " " + needle + pad)
        done = True
        continue
    out.append(line)
if not done:
    out.append("\n[include]\nfiles = " + needle + "\n")
p.write_text("".join(out))
PY
fi

supervisorctl -c "$SUP_CONF" reread
supervisorctl -c "$SUP_CONF" update
if supervisorctl -c "$SUP_CONF" status panrouter 2>/dev/null | grep -q RUNNING; then
  supervisorctl -c "$SUP_CONF" restart panrouter
else
  supervisorctl -c "$SUP_CONF" start panrouter || supervisorctl -c "$SUP_CONF" restart panrouter
fi
supervisorctl -c "$SUP_CONF" start panrouter-backup || true

for i in $(seq 1 30); do
  if curl -fsS --noproxy '*' http://127.0.0.1:6400/healthz | grep -q '"status":"ok"'; then
    echo "healthz ok ($i)"
    supervisorctl -c "$SUP_CONF" status panrouter panrouter-backup || true
    exit 0
  fi
  sleep 1
done

echo "healthz timeout" >&2
supervisorctl -c "$SUP_CONF" status panrouter || true
tail -n 80 /data/panrouter/logs/err.log || true
exit 1
