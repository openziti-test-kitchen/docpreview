#!/usr/bin/env bash
set -euo pipefail
cd /d/worktrees/tangents/vercel-replacement
export NFPREVIEW_ZITI_HOST_IDENTITY='D:\worktrees\tangents\vercel-replacement\demo\ziti-trial\nfpreview-host.json'
export NFPREVIEW_ZITI_READER_IDENTITY='D:\worktrees\tangents\vercel-replacement\demo\ziti-trial\test-reader.json'
go test ./internal/expose/ -count=1 -v -run Ziti -timeout 5m
