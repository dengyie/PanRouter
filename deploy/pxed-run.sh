#!/bin/bash
# pxed 生产入口(NFS 控制面副本)。首次 bootstrap 会把同内容写到
# /personal/pxed/panrouter/run.sh;仓库内这份不含密钥。
set -euo pipefail
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy no_proxy NO_PROXY
set -a
# shellcheck disable=SC1091
source /personal/pxed/panrouter/env
set +a
exec /data/panrouter/bin/panrouter -config /personal/pxed/panrouter/config.yaml
