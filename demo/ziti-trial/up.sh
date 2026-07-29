#!/usr/bin/env bash
# Start a throwaway OpenZiti controller + router natively on this host.
#
# Native rather than Docker on purpose: Ziti Desktop Edge runs on this same
# Windows host, so advertising "localhost" means the tunneler can reach both the
# controller and the router with no port publishing, no host.docker.internal,
# and no advertised-address gymnastics.
set -euo pipefail

ZITI=/c/Users/claude/.ziti/bin/v2.0.0/ziti.exe
HOME_DIR='D:\worktrees\tangents\vercel-replacement\demo\ziti-trial\home'

mkdir -p /d/worktrees/tangents/vercel-replacement/demo/ziti-trial/home

exec "$ZITI" edge quickstart \
  --home "$HOME_DIR" \
  --ctrl-address localhost \
  --ctrl-port 1280 \
  --router-address localhost \
  --router-port 3022 \
  --username admin \
  --password admin
