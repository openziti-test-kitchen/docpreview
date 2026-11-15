#!/usr/bin/env bash
#
# Run every offline test: the Go suite and the dashboard's jsdom tests.
#
# Runnable locally, which is the point: the GitHub Actions workflow does nothing but check out the
# code and call this, so a red pull request can be reproduced on a laptop.
#
#   ./test.sh
#
# Needs go, node and npm, and pwsh for the dashboard tests. The Go tests that need docker skip
# themselves when docker is missing; the release build runs on a host that has it.
set -euo pipefail

cd "$(dirname "$0")"

echo "-- go vet"
go vet ./cmd/... ./internal/...

echo "-- go test"
# Not `./...`: demo/node_modules contains Go files belonging to other modules, and the build fails
# outright on them.
go test ./cmd/... ./internal/...

echo "-- dashboard tests"
pwsh -NoProfile -File ./Test-Dashboard.ps1 -Clean
