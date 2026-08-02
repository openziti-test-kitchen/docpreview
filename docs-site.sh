#!/usr/bin/env bash
#
# Build www/ for a fixed public address, which is what GitHub Pages serves.
#
# Runnable locally, which is the point: the GitHub Actions workflow does nothing but check out the
# code and call this. A publish that only exists inside CI is one nobody can reproduce when CI is
# the thing that is broken.
#
#   ./docs-site.sh                                        # the project's own Pages address
#   ./docs-site.sh https://example.com /                  # a root-mounted site elsewhere
#   OUT=/tmp/site ./docs-site.sh                          # copy the output somewhere else
#
# Docusaurus bakes baseUrl into every href and src at build time, so a site built for one mount
# point cannot be served at another: index.html loads and every stylesheet 404s, with nothing in
# the build log to say why. Both halves of the address are therefore arguments rather than
# constants, and docusaurus.config.ts reads them from the environment.
#
# Output is www/build, copied to $OUT when that is set.
set -euo pipefail

# Git Bash rewrites anything that looks like an absolute POSIX path into a Windows one before
# handing it to a native program, so a base URL of /docpreview/ reaches Docusaurus as
# C:/Program Files/Git/docpreview/ and every link on the site is built against it. The build
# succeeds and the output is unusable. Both variables disable that translation, and neither
# means anything on Linux, where this also runs.
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

cd "$(dirname "${BASH_SOURCE[0]}")"

# A project site lives under the repository name, so the base URL is /docpreview/ rather than /.
# Both values must agree with the repository the Pages site is published from, or every internal
# link resolves to a path that does not exist.
SITE_URL="${1:-https://openziti-test-kitchen.github.io}"
BASE_URL="${2:-/docpreview/}"
OUT="${OUT:-}"

# Docusaurus requires the trailing slash and does not add one. Without it every generated link
# loses the separator and the site 404s wholesale.
case "$BASE_URL" in
  */) ;;
  *) BASE_URL="$BASE_URL/" ;;
esac

echo "url      : $SITE_URL"
echo "base url : $BASE_URL"

cd www

# npm ci rather than npm install: the lockfile is the input, and a resolution that drifts between
# a laptop and CI is a difference nobody sees until the published site differs from the reviewed one.
if [ ! -d node_modules ] || [ "${CLEAN:-}" = "1" ]; then
  echo "-- npm ci"
  npm ci --no-audit --no-fund
fi

echo "-- docusaurus build"
DOCUSAURUS_URL="$SITE_URL" DOCUSAURUS_BASE_URL="$BASE_URL" npm run build

# onBrokenLinks is 'throw', so reaching here means every internal link resolved. That is the
# check worth having: the prefix for the same target differs between docs/ and docs/runbooks/,
# so a link copied from one to the other is wrong without looking wrong.
echo
echo "built www/build for ${SITE_URL}${BASE_URL}"

if [ -n "$OUT" ]; then
  rm -rf "$OUT"
  mkdir -p "$OUT"
  cp -r build/. "$OUT"/
  echo "copied to $OUT"
fi
