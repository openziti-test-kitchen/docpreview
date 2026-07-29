#!/usr/bin/env bash
# An enrolled reader identity for the automated check.
#
# Separate from reviewer-alice on purpose: an OTT enrollment token is
# single-use, so enrolling alice's here would consume the JWT meant for Ziti
# Desktop Edge.
set -euo pipefail

ZITI=/c/Users/claude/.ziti/bin/v2.0.0/ziti.exe
OUT=/d/worktrees/tangents/vercel-replacement/demo/ziti-trial

"$ZITI" edge login localhost:1280 -u admin -p admin -y

if "$ZITI" edge list identities 'name="test-reader"' | grep -q test-reader; then
  "$ZITI" edge delete identity test-reader
fi

"$ZITI" edge create identity test-reader \
  --role-attributes docpreview-reader \
  --jwt-output-file "$OUT/test-reader.jwt"

rm -f "$OUT/test-reader.json"
"$ZITI" edge enroll "$OUT/test-reader.jwt" --out "$OUT/test-reader.json"

echo "enrolled: $OUT/test-reader.json"
