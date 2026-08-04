#!/bin/sh
# Reload systemd, and leave the data alone.
#
# **The data directory, the vault and the config are never removed.** /var/lib/docpreview holds the
# age-encrypted vault and the sqlite database, and /etc/docpreview holds the config that names the
# master key's source. An uninstall that deleted them would destroy every credential the operator
# stored, with nothing to restore from and no warning — and a package removal is something people do
# to upgrade across a broken dependency, not only to say goodbye.
#
# The service account is left in place for the same reason: removing it orphans the files above.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

# This also runs during an upgrade, when the old version is cleaned up after the new one is in
# place. Announcing "removed" there is alarming and false, so both packagers are asked which it is:
# rpm passes the number of remaining instances as $1 and 0 means the last one is going, while dpkg
# passes the literal word "upgrade".
is_upgrade=0
case "${1:-}" in
    upgrade | failed-upgrade) is_upgrade=1 ;;
    [1-9]*) is_upgrade=1 ;;
esac

if [ "$is_upgrade" = "1" ]; then
    exit 0
fi

cat <<'KEPT'
docpreview: removed. Left in place on purpose:

  /var/lib/docpreview   the vault, the database, artifacts and build logs
  /etc/docpreview       the config, which names where the master key comes from
  the docpreview user   owns both of the above

Delete them by hand once you are certain. The vault cannot be recovered without its master key,
and the master key is not in either directory.
KEPT
