#!/bin/sh
set -eu
# No recursive chown: existing private keys and operator-managed state keep their modes.
install -d -o carillon -g carillon -m 0750 /var/lib/carillon
install -d -o root -g carillon -m 0750 /etc/carillon
systemd-tmpfiles --create /usr/lib/tmpfiles.d/carillon.conf
if [ -f /etc/carillon/carillon.toml ]; then
    chown root:carillon /etc/carillon/carillon.toml
    chmod 0640 /etc/carillon/carillon.toml
fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
fi
# Activation and restart are operator decisions, including after an upgrade.
