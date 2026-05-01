#!/usr/bin/env bash
# Build Diego release tarball.
# Run from diego-release directory.
#
# Requires: BOSH CLI, blobstore access. Sync blobs first:
#   bosh sync-blobs
# (Configure config/private.yml with blobstore credentials if needed.)
#
# Usage:
#   ./build-diego-release.sh
#       Builds with bosh --timestamp-version (unique dev version each run).
#   ./build-diego-release.sh <diego-release-version> [tarball-name.tgz]
#       Fixed semver; optional second arg sets tarball path (default diego-<version>.tgz).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [[ -n "${1:-}" ]]; then
  VERSION="$1"
  TARBALL="${2:-diego-${VERSION}.tgz}"
  echo "SCRIPT_DIR = $SCRIPT_DIR"
  echo "VERSION    = $VERSION (explicit)"
  echo "TARBALL    = $TARBALL"
  if [[ -f "$TARBALL" ]]; then
    echo "Tarball already exists: $TARBALL"
    echo "Remove it or pass a different tarball name as arg 2. sha1: $(sha1sum "$TARBALL" | awk '{print $1}')"
    exit 0
  fi
  echo "Building Diego release $VERSION (ensure blobs: bosh sync-blobs) ..."
  bosh create-release --force --version="$VERSION" --tarball="$TARBALL"
else
  TARBALL="diego-dev-$(date -u +%Y%m%d%H%M%S).tgz"
  echo "SCRIPT_DIR = $SCRIPT_DIR"
  echo "MODE       = timestamp-version (no fixed version; avoids 'already exists')"
  echo "TARBALL    = $TARBALL"
  if [[ -f "$TARBALL" ]]; then
    echo "Tarball already exists (unlikely): $TARBALL"
    exit 0
  fi
  echo "Building Diego release with --timestamp-version (ensure blobs: bosh sync-blobs) ..."
  bosh create-release --force --timestamp-version --tarball="$TARBALL"
fi

echo "Created $TARBALL"
echo "sha1: $(sha1sum "$TARBALL" | awk '{print $1}')"
echo ""
echo "Next: point DIEGO_TGZ at this tarball and run ./update-10.2.7-tile.sh ..."
