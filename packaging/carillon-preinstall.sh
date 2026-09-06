#!/bin/sh
set -eu
# Create identities before RPM resolves file ownership; never remove them on erase.
getent group carillon >/dev/null || groupadd --system carillon
if ! getent passwd carillon >/dev/null; then
    useradd --system --gid carillon --home-dir /var/lib/carillon \
        --no-create-home --shell /sbin/nologin carillon
fi
