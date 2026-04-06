#!/usr/bin/env bash
# Build Diego release 2.128.0+dev.14 tarball (droplet cache on persistent disk).
# Run from diego-release directory.
#
# Requires: BOSH CLI, blobstore access. Sync blobs first:
#   bosh sync-blobs
# (Configure config/private.yml with blobstore credentials if needed.)
#
# Usage: ./build-diego-dev14.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"
VERSION="2.130.0+cache-on-disk.26"
TARBALL="diego-${VERSION}.tgz"

if [[ -f "$TARBALL" ]]; then
  echo "Tarball already exists: $TARBALL"
  echo "Remove it to rebuild. sha1: $(sha1sum "$TARBALL" | awk '{print $1}')"
  exit 0
fi

echo "Building Diego release $VERSION (this may take a while; ensure blobs are synced: bosh sync-blobs) ..."
bosh create-release --force --version="$VERSION" --tarball="$TARBALL"

echo "Created $TARBALL"
echo "sha1: $(sha1sum "$TARBALL" | awk '{print $1}')"
echo ""
echo "Next: build the tile with ./update-tile-to-dev14.sh"
