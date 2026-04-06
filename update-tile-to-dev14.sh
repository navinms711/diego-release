#!/usr/bin/env bash
# Update a 10.2.7 tile to use Diego build.26 AND nfs-volume 7.55.0+dev.1
# Usage: ./update-tile-to-dev14.sh [path-to-base-tile] [path-to-output-tile]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE="$(cd "$SCRIPT_DIR/.." && pwd)"
INPUT_TILE="${1:-/tmp/cf-10.2.7-build.2.pivotal}"
OUTPUT_TILE="${2:-$WORKSPACE/cf-10.2.7-freshness-droplet-cache-build.26.pivotal}"
METADATA_DEV14="$WORKSPACE/upgrade-6.0.9-to-10.2.7/metadata-10.2.7-dev14.yml"

# --- FIX 1: Point to build.26 ---
DIEGO_TGZ="$SCRIPT_DIR/diego-2.130.0+cache-on-disk.26.tgz"
NFS_TGZ="/tmp/nfs-volume-dev.tgz"
OUTPUT_DIR="$(dirname "$OUTPUT_TILE")"
export TMPDIR="${OUTPUT_DIR}"
TILE_EXTRACT="${OUTPUT_DIR}/.tile-extract-$$"

cleanup() {
  rm -rf "$TILE_EXTRACT"
}
trap cleanup EXIT

# --- FIX 2: Check that NFS_TGZ exists ---
if [[ ! -f "$INPUT_TILE" ]]; then
  echo "Error: Base tile not found: $INPUT_TILE"
  exit 1
fi
if [[ ! -f "$METADATA_DEV14" ]]; then
  echo "Error: Metadata not found: $METADATA_DEV14"
  exit 1
fi
if [[ ! -f "$DIEGO_TGZ" ]]; then
  echo "Error: Diego tarball not found: $DIEGO_TGZ"
  exit 1
fi
if [[ ! -f "$NFS_TGZ" ]]; then
  echo "Error: NFS tarball not found: $NFS_TGZ"
  exit 1
fi

NEW_DIEGO_SHA1=$(sha1sum "$DIEGO_TGZ" | awk '{print $1}')
NEW_NFS_SHA1=$(sha1sum "$NFS_TGZ" | awk '{print $1}')

METADATA_TMP="${TMPDIR}/metadata-10.2.7-dev14-$$.yml"
sed "s/REPLACE_ME_DEV14/$NEW_DIEGO_SHA1/" "$METADATA_DEV14" | \
sed "s/REPLACE_ME_NFS_DEV/$NEW_NFS_SHA1/" > "$METADATA_TMP"

DIEGO_IN_TILE=$(unzip -l "$INPUT_TILE" | awk '$4 ~ /^releases\/diego-.*\.tgz$/ { print $4 }' | head -1)

# --- FIX 3: Discover old NFS release in tile ---
NFS_IN_TILE=$(unzip -l "$INPUT_TILE" | awk '$4 ~ /^releases\/nfs-volume-.*\.tgz$/ { print $4 }' | head -1)

echo "Replacing in tile: $DIEGO_IN_TILE and $NFS_IN_TILE"
echo "Output tile: $OUTPUT_TILE"
echo ""

echo "Extracting tile ..."
mkdir -p "$TILE_EXTRACT"
unzip -q -o "$INPUT_TILE" -d "$TILE_EXTRACT"

# --- FIX 3: Remove BOTH old releases ---
echo "Removing old diego/nfs releases and metadata ..."
rm -f "$TILE_EXTRACT/$DIEGO_IN_TILE" "$TILE_EXTRACT/$NFS_IN_TILE" "$TILE_EXTRACT/metadata/metadata.yml"

echo "Adding metadata for build.26 ..."
mkdir -p "$TILE_EXTRACT/metadata"
cp "$METADATA_TMP" "$TILE_EXTRACT/metadata/metadata.yml"

echo "Adding diego-2.130.0+cache-on-disk.26.tgz ..."
mkdir -p "$TILE_EXTRACT/releases"
cp "$DIEGO_TGZ" "$TILE_EXTRACT/releases/diego-2.130.0+cache-on-disk.26.tgz"

echo "Adding nfs-volume-7.55.0+dev.1.tgz ..."
cp "$NFS_TGZ" "$TILE_EXTRACT/releases/nfs-volume-7.55.0+dev.1.tgz"

echo "Creating new tile archive ..."
(cd "$TILE_EXTRACT" && zip -r -q "$OUTPUT_TILE" .)

echo "Done. Output tile: $OUTPUT_TILE"
echo ""
echo "Verify with:"
echo "  unzip -l $OUTPUT_TILE | grep diego"
echo "  unzip -p $OUTPUT_TILE metadata/metadata.yml | grep -A4 'name: diego'"
echo ""
echo "This tile uses Diego dev.14 with droplet cache on persistent disk (/var/vcap/store/rep_download_cache)."
