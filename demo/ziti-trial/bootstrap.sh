#!/usr/bin/env bash
# Create everything the tunneler-only preview design calls for, exactly once.
#
# This is the hand-rolled version of what `nfpreview identity bootstrap` would
# do. Six objects, none of them per-pull-request:
#
#   1. an intercept.v1 config covering *.nfpreview.ziti
#   2. one wildcard service carrying it
#   3. a Bind policy, so nfpreview may host the service
#   4. a Dial policy keyed on a role attribute, so adding a reviewer later is
#      one attribute rather than a policy edit
#   5. a service-edge-router policy
#   6. nfpreview's own hosting identity, enrolled here into an identity file
#
# Plus one reviewer identity, whose enrollment JWT is what gets imported into
# Ziti Desktop Edge.
set -euo pipefail

ZITI=/c/Users/claude/.ziti/bin/v2.0.0/ziti.exe
OUT=/d/worktrees/tangents/vercel-replacement/demo/ziti-trial
DOMAIN=nfpreview.ziti

say() { printf '\n=== %s\n' "$*"; }

say "login"
"$ZITI" edge login localhost:1280 -u admin -p admin -y

# Idempotence: this script is run more than once while iterating, and the
# controller rejects duplicate names rather than treating them as no-ops.
drop() {
  local kind=$1 name=$2
  if "$ZITI" edge list "$kind" "name=\"$name\"" | grep -q "$name"; then
    echo "removing existing $kind $name"
    "$ZITI" edge delete "$kind" "$name"
  fi
}

say "clearing anything from a previous run"
drop service-policies nfpreview-dial
drop service-policies nfpreview-bind
drop service-edge-router-policies nfpreview-serp
drop services nfpreview-svc
drop configs nfpreview-intercept
drop identities nfpreview-host
drop identities reviewer-alice

say "1. intercept.v1 config"
# The apex is listed alongside the wildcard because the matcher keys on the
# suffix: *.nfpreview.ziti matches foo.nfpreview.ziti but not the bare name.
"$ZITI" edge create config nfpreview-intercept intercept.v1 \
  "{\"protocols\":[\"tcp\"],\"addresses\":[\"$DOMAIN\",\"*.$DOMAIN\"],\"portRanges\":[{\"low\":80,\"high\":80}]}"

say "2. the one wildcard service"
"$ZITI" edge create service nfpreview-svc --configs nfpreview-intercept -e ON

say "6a. nfpreview's hosting identity"
"$ZITI" edge create identity nfpreview-host \
  --role-attributes nfpreview-host \
  --jwt-output-file "$OUT/nfpreview-host.jwt"

say "3. Bind policy — nfpreview may host the service"
"$ZITI" edge create service-policy nfpreview-bind Bind \
  --identity-roles '#nfpreview-host' \
  --service-roles '@nfpreview-svc'

say "4. Dial policy — anyone carrying the reader attribute may reach it"
"$ZITI" edge create service-policy nfpreview-dial Dial \
  --identity-roles '#nfpreview-reader' \
  --service-roles '@nfpreview-svc'

say "5. service-edge-router policy"
"$ZITI" edge create service-edge-router-policy nfpreview-serp \
  --edge-router-roles '#all' \
  --service-roles '@nfpreview-svc'

say "6b. a reviewer identity — this JWT goes into Ziti Desktop Edge"
"$ZITI" edge create identity reviewer-alice \
  --role-attributes nfpreview-reader \
  --jwt-output-file "$OUT/reviewer-alice.jwt"

say "enrolling nfpreview's own identity"
rm -f "$OUT/nfpreview-host.json"
"$ZITI" edge enroll "$OUT/nfpreview-host.jwt" --out "$OUT/nfpreview-host.json"

say "what exists now"
"$ZITI" edge list services
"$ZITI" edge list identities
"$ZITI" edge list service-policies

say "done"
echo "  hosting identity : $OUT/nfpreview-host.json"
echo "  reviewer JWT     : $OUT/reviewer-alice.jwt"
