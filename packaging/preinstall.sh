#!/bin/sh
# Create the service account before any file is placed, because the package declares directories
# owned by it and the ownership is applied as they are unpacked.
#
# Idempotent: this runs again on every upgrade, and a package that failed here would leave the
# installation half-applied.
set -e

if ! getent passwd nfpreview >/dev/null 2>&1; then
    # Home is the data directory rather than /home/nfpreview. This installation points zrok at
    # the data directory, and keeping the home there means `sudo -u nfpreview zrok2 ...` behaves
    # for anyone who reaches for it.
    useradd --system --create-home --home-dir /var/lib/nfpreview \
        --shell /usr/sbin/nologin nfpreview 2>/dev/null ||
        useradd --system --home-dir /var/lib/nfpreview --shell /sbin/nologin nfpreview
fi
