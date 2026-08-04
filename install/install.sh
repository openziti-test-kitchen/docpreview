#!/usr/bin/env bash
#
# Install docpreview as a systemd service on a fresh Linux VM.
#
# It does the boring, get-it-wrong-once parts: the service account, the directories and their
# permissions, the binary, the unit files. It deliberately does **not** write a config, mint a
# master key, or store a credential — those are decisions, and a script that makes them for you is
# a script whose choices nobody knows they inherited.
#
# What it leaves you with is a stopped service and a printed list of the four commands that start
# it. See www/docs/guides/linux-service.md for the whole path including zrok and the App.
#
#   sudo ./install.sh --version v0.3.0      # download a release and verify its checksum
#   sudo ./install.sh                       # binary from ./build or $PWD
#   sudo ./install.sh --binary /tmp/docpreview
#   sudo ./install.sh --uninstall           # units and user; keeps the data
#
set -euo pipefail

USER_NAME=docpreview
DATA_DIR=/var/lib/docpreview
CONF_DIR=/etc/docpreview
BIN_DEST=/usr/local/bin/docpreview
UNIT_DIR=/etc/systemd/system
REPO="${DOCPREVIEW_REPO:-openziti-test-kitchen/docpreview}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

binary=""
release=""
uninstall=0

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) binary="$2"; shift 2 ;;
    --version) release="$2"; shift 2 ;;
    --uninstall) uninstall=1; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "run this with sudo: it creates a user, writes to /usr/local/bin and installs units" >&2
  exit 1
fi

say() { printf '\n\033[36m-- %s\033[0m\n' "$1"; }

# ── Uninstall ────────────────────────────────────────────────────────────────────────────────
#
# The data directory is never removed. It holds the vault, the database and the zrok enrolment —
# an uninstall script that deletes an account's only enrolled identity, and every reserved name
# with it, is not a thing to write.
if [ "$uninstall" -eq 1 ]; then
  say "stopping and removing the units"
  for unit in docpreview-dashboard docpreview-webhook docpreview; do
    systemctl disable --now "$unit.service" 2>/dev/null || true
    rm -f "$UNIT_DIR/$unit.service"
  done
  systemctl daemon-reload
  rm -f "$BIN_DEST"
  echo
  echo "Removed the units and the binary."
  echo "Kept $DATA_DIR and $CONF_DIR — the vault, the database and the zrok enrolment are in them."
  echo "Remove them by hand if you mean to, and read the zrok runbook first: the enrolment owns"
  echo "every reserved name on that account and nothing else can release them."
  exit 0
fi

# ── The binary ───────────────────────────────────────────────────────────────────────────────

# download_release fetches a published build and **verifies it against SHA256SUMS** before anything
# is unpacked.
#
# The verification is the point, not a formality. This script is the kind of thing that ends up
# being piped from a URL into a shell, and an unverified download in it is an invitation: whoever
# can interfere with one of these two requests gets to choose what runs as root on this host.
# Checking is three lines and removes that entirely — provided the failure is fatal, which is why
# every step below exits rather than warning.
download_release() {
  local version="$1" arch tmp name
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "no release build for $(uname -m); build from source" >&2; exit 1 ;;
  esac

  name="docpreview_${version}_linux_${arch}.tar.gz"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local base="https://github.com/$REPO/releases/download/$version"
  say "downloading $name"
  curl -fSL --proto '=https' --tlsv1.2 -o "$tmp/$name" "$base/$name" || {
    echo "could not download $base/$name" >&2
    echo "check the version exists: https://github.com/$REPO/releases" >&2
    exit 1
  }
  curl -fSL --proto '=https' --tlsv1.2 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || {
    echo "the release has no SHA256SUMS; refusing to install an unverified binary" >&2
    exit 1
  }

  say "verifying the checksum"
  # Only this file's line, so a SHA256SUMS naming other archives does not fail the check for
  # things that were never downloaded.
  #
  # `[ *]` before the name: sha256sum writes `hash  name` on Linux and `hash *name` in binary
  # mode, and matching one spelling would verify half the releases and reject the other half.
  ( cd "$tmp" && grep -E "[ *]${name}\$" SHA256SUMS > one.sha256 && sha256sum -c one.sha256 ) || {
    echo >&2
    echo "CHECKSUM MISMATCH. Not installing." >&2
    echo "The download does not match the checksum published with the release. Either it was" >&2
    echo "corrupted in transit, or it is not the file the release says it is." >&2
    exit 1
  }

  tar -xzf "$tmp/$name" -C "$tmp"
  binary="$tmp/docpreview_${version}_linux_${arch}/docpreview"
  [ -x "$binary" ] || { echo "the archive did not contain a docpreview binary" >&2; exit 1; }

  # The units too, so a downloaded install does not need the repository checked out beside it.
  if [ -d "$tmp/docpreview_${version}_linux_${arch}/install" ]; then
    HERE="$tmp/docpreview_${version}_linux_${arch}/install"
  fi
}

if [ -n "$release" ]; then
  download_release "$release"
fi

# Found locally when no version was asked for: the build you just made, or the one beside this
# script.
if [ -z "$binary" ]; then
  for candidate in "$HERE/../build/docpreview" "$HERE/docpreview" "$PWD/docpreview"; do
    if [ -x "$candidate" ]; then binary="$candidate"; break; fi
  done
fi
if [ -z "$binary" ] || [ ! -x "$binary" ]; then
  cat >&2 <<EOF
No docpreview binary found. Three ways to get one:

    sudo ./install.sh --version v0.3.0          # a published release, checksum verified
    https://github.com/$REPO/releases           # the list of them

    go build -o /tmp/docpreview ./cmd/docpreview
    sudo ./install.sh --binary /tmp/docpreview

    GOOS=linux GOARCH=amd64 go build -o docpreview ./cmd/docpreview   # from a laptop
EOF
  exit 1
fi

say "checking the binary runs on this host"
"$binary" help >/dev/null 2>&1 || {
  echo "that binary did not run here — wrong architecture, or not docpreview" >&2
  exit 1
}

# ── The service account ──────────────────────────────────────────────────────────────────────
say "creating the $USER_NAME service account"
if id "$USER_NAME" >/dev/null 2>&1; then
  echo "   already exists"
else
  # A home directory, which is not decoration: zrok keeps its environment under the *user's*
  # home by default, and a service account with no home means an enrolment nobody can find.
  # This installation points zrok at the data directory instead, and the home is still here so
  # that `sudo -u docpreview zrok2 ...` behaves if anyone reaches for it.
  useradd --system --create-home --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$USER_NAME"
  echo "   created, home $DATA_DIR"
fi

if getent group docker >/dev/null 2>&1; then
  say "adding $USER_NAME to the docker group"
  usermod -aG docker "$USER_NAME"
  echo "   done — note this is root-equivalent on this host, see the runbook"
else
  say "no docker group on this host"
  echo "   builds will need build.driver: local, which runs npm directly on this machine"
fi

# ── Directories ──────────────────────────────────────────────────────────────────────────────
say "creating directories"
install -d -o "$USER_NAME" -g "$USER_NAME" -m 0700 "$DATA_DIR"
# Root-owned and group-writable. Step 1 below is `sudo -u docpreview docpreview init -config
# $CONF_DIR/config.yml`, which writes a temporary file here before renaming it into place — at 0750
# that fails with a permission denied on a name nobody recognises, and it reads as a broken install
# rather than as a directory mode. The same account writes the master key here in step 2.
#
# Not world-readable: the config names where the master key comes from.
install -d -o root -g "$USER_NAME" -m 0770 "$CONF_DIR"
echo "   $DATA_DIR (0700, $USER_NAME)"
echo "   $CONF_DIR (0770, root:$USER_NAME)"

# ── The binary and the units ─────────────────────────────────────────────────────────────────
say "installing the binary"
install -o root -g root -m 0755 "$binary" "$BIN_DEST"
echo "   $BIN_DEST  ($("$BIN_DEST" help 2>&1 | head -n 1))"

say "installing the units"
for unit in docpreview docpreview-webhook docpreview-dashboard; do
  install -o root -g root -m 0644 "$HERE/$unit.service" "$UNIT_DIR/$unit.service"
  echo "   $UNIT_DIR/$unit.service"
done
systemctl daemon-reload

# ── What is left, which is the part that needs decisions ─────────────────────────────────────
cat <<EOF

Installed. Nothing is running yet, and four things are still yours to decide.

1. A config file. One question, and it writes commented YAML:

     sudo -u $USER_NAME $BIN_DEST init -config $CONF_DIR/config.yml

   Then set data_dir to $DATA_DIR in it.

2. A master key, so restarts do not need a person:

     sudo -u $USER_NAME $BIN_DEST vault keygen -out $CONF_DIR/master.key

   keygen writes it 0600 and owned by $USER_NAME, which is what the daemon requires. Do not
   widen it: a key file readable by group or other is refused at startup, because it decrypts
   every secret in the vault.

   and in config.yml:

     vault:
       key_source: "file:$CONF_DIR/master.key"

3. A way onto the internet. Sign up for zrok and enrol this host — as the service account, or
   the enrolment lands in the wrong home directory:

     sudo -u $USER_NAME $BIN_DEST zrok use project -config $CONF_DIR/config.yml
     sudo -u $USER_NAME $BIN_DEST zrok invite you@example.com -config $CONF_DIR/config.yml
     sudo -u $USER_NAME $BIN_DEST zrok register <the-link-from-that-email> -config $CONF_DIR/config.yml

4. Passwords on the dashboard, before you expose it:

     sudo -u $USER_NAME $BIN_DEST console password -role admin  -config $CONF_DIR/config.yml
     sudo -u $USER_NAME $BIN_DEST console password -role viewer -config $CONF_DIR/config.yml

Then:

     sudo -u $USER_NAME $BIN_DEST doctor -config $CONF_DIR/config.yml
     sudo systemctl enable --now docpreview
     sudo systemctl enable --now docpreview-webhook
     journalctl -u docpreview -f

The dashboard share is deliberately not enabled: it publishes the operator dashboard, which lists
every open documentation pull request. Set the viewer password first, then:

     sudo systemctl enable --now docpreview-dashboard

Full walkthrough: www/docs/guides/linux-service.md
EOF
