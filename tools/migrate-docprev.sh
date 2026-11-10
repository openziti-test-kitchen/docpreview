#!/usr/bin/env bash
#
# Drive install/migrate-from-docpreview.sh on a host from this workstation, over ssh.
#
#   tools/migrate-docprev.sh upload [dist]    copy the rpm, SHA256SUMS and the host script to the host
#   tools/migrate-docprev.sh check            read-only
#   tools/migrate-docprev.sh stage            copy the build caches; docpreview keeps running
#   OLD_SHARES=adopt tools/migrate-docprev.sh cutover
#   tools/migrate-docprev.sh verify           read-only
#   tools/migrate-docprev.sh rollback
#   tools/migrate-docprev.sh finish
#
# HOST is the ssh alias, docprev by default. dist is release.sh's output directory, dist/ by
# default. Every step but upload first checks that the host's copy of the script is this one, so
# a step never runs a script nobody reviewed. Each run is also logged to $LOGDIR (default
# build.claude/migration-logs).
#
# Env vars the host script reads are passed through when set: OLD_SHARES, WEBHOOK_NAME, DASH_NAME,
# COPY_CACHES, ENABLE_AT_BOOT, NEW_RPM, OLD_RPM, HEALTH_WAIT.
#
# Runbook: www/docs/guides/migrate-from-docpreview.md.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

HOST="${HOST:-docprev}"
REMOTE_DIR=/var/tmp/nfpreview-migration
SCRIPT=install/migrate-from-docpreview.sh
LOGDIR="${LOGDIR:-build.claude/migration-logs}"

step="${1:-}"
[ -n "$step" ] || { sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

local_sha() { sha256sum "$1" | cut -d' ' -f1; }
remote_sha() { ssh "$HOST" "sha256sum '$1' 2>/dev/null | cut -d' ' -f1"; }

if [ "$step" = upload ]; then
  dist="${2:-dist}"
  rpms=("$dist"/nfpreview-*.x86_64.rpm)
  [ "${#rpms[@]}" = 1 ] && [ -f "${rpms[0]}" ] || { echo "need exactly one nfpreview-*.x86_64.rpm in $dist" >&2; exit 1; }
  rpm="${rpms[0]}"
  # SHA256SUMS lists every artifact; the host only gets the line for the rpm it receives.
  grep -E "[ *]$(basename "$rpm")\$" "$dist/SHA256SUMS" > "$dist/SHA256SUMS.rpm" ||
    { echo "$(basename "$rpm") is not in $dist/SHA256SUMS" >&2; exit 1; }
  ssh "$HOST" "mkdir -p $REMOTE_DIR && chmod 0755 $REMOTE_DIR"
  scp -q "$rpm" "$SCRIPT" "$HOST:$REMOTE_DIR/"
  scp -q "$dist/SHA256SUMS.rpm" "$HOST:$REMOTE_DIR/SHA256SUMS"
  rm -f "$dist/SHA256SUMS.rpm"
  for f in "$rpm" "$SCRIPT"; do
    want="$(local_sha "$f")"
    got="$(remote_sha "$REMOTE_DIR/$(basename "$f")")"
    [ "$want" = "$got" ] || { echo "sha256 mismatch on the host for $(basename "$f")" >&2; exit 1; }
    echo "uploaded  $want  $(basename "$f")"
  done
  exit 0
fi

case "$step" in
  check | stage | cutover | verify | rollback | finish) ;;
  *) echo "unknown step: $step" >&2; exit 2 ;;
esac

want="$(local_sha "$SCRIPT")"
got="$(remote_sha "$REMOTE_DIR/$(basename "$SCRIPT")")"
[ "$want" = "$got" ] || { echo "the host's $SCRIPT differs from this checkout's; run: $0 upload" >&2; exit 1; }

envs=()
for v in OLD_SHARES WEBHOOK_NAME DASH_NAME COPY_CACHES ENABLE_AT_BOOT NEW_RPM OLD_RPM HEALTH_WAIT; do
  if [ -n "${!v+x}" ]; then envs+=("$v=$(printf '%q' "${!v}")"); fi
done

mkdir -p "$LOGDIR"
log="$LOGDIR/$HOST-$step-$(date -u +%Y%m%dT%H%M%SZ).log"
echo "logging to $log"
# -t so systemctl and rpm see a terminal and the run can be interrupted from here.
ssh -t "$HOST" "sudo env ${envs[*]:-} bash $REMOTE_DIR/$(basename "$SCRIPT") $step" 2>&1 | tee "$log"
exit "${PIPESTATUS[0]}"
