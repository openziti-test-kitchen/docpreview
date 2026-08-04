#!/bin/sh
# Stop the services before the binary they run goes away.
#
# All three, and failures ignored: a unit that was never enabled is not an error here, and a package
# removal that aborts because a service was already stopped leaves the package half-removed.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop docpreview docpreview-webhook docpreview-dashboard 2>/dev/null || true
    systemctl disable docpreview docpreview-webhook docpreview-dashboard 2>/dev/null || true
fi
