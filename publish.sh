#!/usr/bin/env bash
#
# Publish the artifacts release.sh built as a GitHub release for one tag.
#
# Runnable locally, like release.sh: the workflow only checks out the code and calls this. Needs gh,
# authenticated through GH_TOKEN or `gh auth login`, and the repository's tags.
#
#   ./publish.sh v0.5.1                # publishes dist/ as the v0.5.1 release
#   OUT=/tmp/rel ./publish.sh v0.5.1   # somewhere else
#
# A release is marked Latest only when its tag is the highest release version in the repository. GitHub
# otherwise marks whatever was published last, so backfilling an older tag took the flag from a newer
# release. A tag with a pre-release suffix such as v0.5.1-rc.1 is never Latest.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

TAG="${1:?usage: ./publish.sh <tag>}"
OUT="${OUT:-dist}"

# The highest tag that looks like vMAJOR.MINOR.PATCH and nothing more.
highest="$(git tag --list 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1 || true)"
if [ "$TAG" = "$highest" ]; then
  latest=true
else
  latest=false
fi
echo "tag     : $TAG"
echo "highest : ${highest:-none}"
echo "latest  : $latest"

# --notes-file, then --generate-notes to append the pull request list under the Changelog heading the
# file ends with. Generated notes alone are a list of PR titles, which reads as noise to somebody arriving
# at the release page.
gh release create "$TAG" \
  --title "$TAG" \
  --notes-file "$OUT/NOTES.md" \
  --generate-notes \
  --verify-tag \
  --latest="$latest" \
  "$OUT"/*.tar.gz "$OUT"/*.zip "$OUT"/*.deb "$OUT"/*.rpm "$OUT"/SHA256SUMS
