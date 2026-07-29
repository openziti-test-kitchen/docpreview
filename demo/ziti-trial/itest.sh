#!/usr/bin/env bash
set -euo pipefail
cd /d/worktrees/tangents/vercel-replacement
export DOCPREVIEW_ZITI_HOST_IDENTITY='D:\worktrees\tangents\vercel-replacement\demo\ziti-trial\docpreview-host.json'
export DOCPREVIEW_ZITI_READER_IDENTITY='D:\worktrees\tangents\vercel-replacement\demo\ziti-trial\test-reader.json'
go test ./internal/expose/ -count=1 -v -run Ziti -timeout 5m
