#!/bin/sh
# Register the units and say what to do next. Nothing is started.
#
# Starting a daemon that has no config and no vault would produce a service in a restart loop and a
# journal full of the same error, which is a worse first impression than a service that is installed
# and waiting. `docpreview init` is the next step and it is one command.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

# The docker group is root-equivalent on the host, so this is stated rather than done quietly.
if getent group docker >/dev/null 2>&1; then
    usermod -aG docker docpreview 2>/dev/null || true
    echo "docpreview: added to the docker group — this is root-equivalent on this host"
fi

if [ ! -f /etc/docpreview/config.yml ]; then
    cat <<'NEXT'

docpreview is installed and not running. Three steps:

  1. sudo -u docpreview docpreview init -config /etc/docpreview/config.yml
  2. sudo -u docpreview docpreview doctor -config /etc/docpreview/config.yml
  3. sudo systemctl enable --now docpreview

The two tunnel units are separate, and only needed once a zrok environment is enabled here:

     sudo systemctl enable --now docpreview-webhook docpreview-dashboard

  https://openziti-test-kitchen.github.io/docpreview/docs/guides/linux-service

NEXT
else
    echo "docpreview: upgraded. Existing config, vault and database left alone."
    echo "docpreview: sudo systemctl restart docpreview to run the new binary."
fi
