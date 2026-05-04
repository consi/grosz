#!/bin/sh
set -e

# Create grosz system user if it doesn't exist
if ! getent group grosz >/dev/null 2>&1; then
    groupadd --system grosz
fi
if ! getent passwd grosz >/dev/null 2>&1; then
    useradd --system --gid grosz --home-dir /var/lib/grosz --shell /usr/sbin/nologin grosz
fi

# Create data directory
mkdir -p /var/lib/grosz
chown grosz:grosz /var/lib/grosz
chmod 750 /var/lib/grosz

# Enable and start service
systemctl daemon-reload
systemctl enable grosz.service
systemctl restart grosz.service
