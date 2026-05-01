#!/usr/bin/env bash
# Repack a 10.2.x CF tile with Diego build.35 + nfs-volume 7.55.0+dev.1 (paths relative to this script by default).
#
# Usage: ./test.sh [path-to-base-tile] [path-to-output-tile] [path-to-metadata-yml]
#
# Requires: base .pivotal, metadata with REPLACE_ME_DIEGO and REPLACE_ME_NFS_DEV placeholders,
#           diego tarball and nfs tarball next to this script (or set DIEGO_REL / NFS_REL).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

INPUT_TILE="${1:-$SCRIPT_DIR/cf-10.2.7-build.2.pivotal}"
OUTPUT_TILE="${2:-$SCRIPT_DIR/cf-10.2.7-freshness-droplet-cache-build.35.pivotal}"
METADATA_DIEGO="${3:-$SCRIPT_DIR/metadata-10.2.7-dev.35.yml}"

# zip runs with cwd=TILE_EXTRACT; relative paths would write inside it and be deleted by cleanup.
if [[ "$INPUT_TILE" != /* ]]; then
  INPUT_TILE="$SCRIPT_DIR/$INPUT_TILE"
fi
if [[ "$OUTPUT_TILE" != /* ]]; then
  OUTPUT_TILE="$SCRIPT_DIR/$OUTPUT_TILE"
fi

DIEGO_REL="${DIEGO_REL:-diego-2.130.0+cache-on-disk.35.tgz}"
DIEGO_TGZ="$SCRIPT_DIR/$DIEGO_REL"
NFS_REL="${NFS_REL:-nfs-volume-7.55.0+dev.1.tgz}"
NFS_TGZ="$SCRIPT_DIR/$NFS_REL"
NFS_IN_TILE_NAME="${NFS_IN_TILE_NAME:-nfs-volume-7.55.0+dev.1.tgz}"

OUTPUT_DIR="$(dirname "$OUTPUT_TILE")"
mkdir -p "$OUTPUT_DIR"
export TMPDIR="${OUTPUT_DIR}"
TILE_EXTRACT="${OUTPUT_DIR}/.tile-extract-$$"

cleanup() {
  rm -rf "$TILE_EXTRACT"
}
trap cleanup EXIT

if [[ ! -f "$INPUT_TILE" ]]; then
  echo "Error: Base tile not found: $INPUT_TILE"
  exit 1
fi
if [[ ! -f "$METADATA_DIEGO" ]]; then
  echo "Error: Metadata not found: $METADATA_DIEGO"
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

METADATA_TMP="${TMPDIR}/metadata-10.2.7-tmp-$$.yml"
sed "s/REPLACE_ME_DIEGO/$NEW_DIEGO_SHA1/" "$METADATA_DIEGO" | \
  sed "s/REPLACE_ME_NFS_DEV/$NEW_NFS_SHA1/" > "$METADATA_TMP"

DIEGO_IN_TILE=$(unzip -l "$INPUT_TILE" | awk '$4 ~ /^releases\/diego-.*\.tgz$/ { print $4 }' | head -1)
NFS_IN_TILE=$(unzip -l "$INPUT_TILE" | awk '$4 ~ /^releases\/nfs-volume-.*\.tgz$/ { print $4 }' | head -1)

if [[ -z "$DIEGO_IN_TILE" || -z "$NFS_IN_TILE" ]]; then
  echo "Error: Could not find old diego or nfs releases inside the base tile zip!"
  exit 1
fi

echo "Replacing in tile: $DIEGO_IN_TILE and $NFS_IN_TILE"
echo "Output tile: $OUTPUT_TILE"
echo ""

echo "Extracting tile ..."
mkdir -p "$TILE_EXTRACT"
unzip -q -o "$INPUT_TILE" -d "$TILE_EXTRACT"

echo "Removing old diego/nfs releases and metadata ..."
rm -f "$TILE_EXTRACT/$DIEGO_IN_TILE" "$TILE_EXTRACT/$NFS_IN_TILE" "$TILE_EXTRACT/metadata/metadata.yml"

echo "Adding metadata for $OUTPUT_TILE ..."
mkdir -p "$TILE_EXTRACT/metadata"
cp "$METADATA_TMP" "$TILE_EXTRACT/metadata/metadata.yml"

echo "Adding $DIEGO_REL ..."
mkdir -p "$TILE_EXTRACT/releases"
cp "$DIEGO_TGZ" "$TILE_EXTRACT/releases/$DIEGO_REL"

echo "Adding $NFS_IN_TILE_NAME ..."
cp "$NFS_TGZ" "$TILE_EXTRACT/releases/$NFS_IN_TILE_NAME"

echo "Creating new tile archive ..."
(cd "$TILE_EXTRACT" && zip -r -q "$OUTPUT_TILE" .)

echo "Done. Output tile: $OUTPUT_TILE"
echo ""
echo "Verify with:"
echo "  unzip -l \"$OUTPUT_TILE\" | grep diego"
echo "  unzip -p \"$OUTPUT_TILE\" metadata/metadata.yml | grep -A4 'name: diego'"
echo ""
echo "This tile uses Diego $DIEGO_REL with droplet cache on persistent disk (/var/vcap/store/rep_download_cache)."
