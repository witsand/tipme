#!/usr/bin/env bash
# Run this ONCE on your Ubuntu/Debian server to bootstrap the tipme service.
# Usage:  sudo bash setup-server.sh

set -euo pipefail

INSTALL_DIR=/opt/tipme
SERVICE_USER=tipme
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ $EUID -ne 0 ]]; then
  echo "✗ Please run as root: sudo bash $0" >&2
  exit 1
fi

echo "── Creating system user '$SERVICE_USER'..."
if ! id "$SERVICE_USER" &>/dev/null; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  echo "  ✓ User created."
else
  echo "  ✓ User already exists."
fi

echo "── Creating $INSTALL_DIR ..."
mkdir -p "$INSTALL_DIR/static"
chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"
echo "  ✓ Directory ready."

echo "── Installing systemd service..."
cp "$SCRIPT_DIR/tipme.service" /etc/systemd/system/tipme.service
systemctl daemon-reload
systemctl enable tipme
echo "  ✓ Service installed and enabled."

# Create a starter .env if one doesn't exist.
if [ ! -f "$INSTALL_DIR/.env" ]; then
  cp "$SCRIPT_DIR/server.env.example" "$INSTALL_DIR/.env"
  chown "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR/.env"
  chmod 600 "$INSTALL_DIR/.env"
  echo "  ✓ Created $INSTALL_DIR/.env from example — edit it before starting."
fi

echo ""
echo "✓ Server setup complete!"
echo ""
echo "Next steps:"
echo "  1. Edit $INSTALL_DIR/.env with your BASE_URL, BLITZI_TOKEN, etc."
echo "  2. From your dev machine run:  deploy"
echo "  3. sudo systemctl start tipme"
echo "  4. Check logs:  journalctl -u tipme -f"
