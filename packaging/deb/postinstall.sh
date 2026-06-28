#!/bin/sh
set -e

# 1. Ensure the default mountpoint exists.
mkdir -p /mnt/stitchfs

# 2. Pick up the shipped unit (do NOT auto-enable; let the admin opt in).
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
fi

echo "stitchfs installed. Start the mount with:"
echo "    sudo systemctl enable --now stitchfs.service"
