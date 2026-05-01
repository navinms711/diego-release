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
#       If that dev version was built before, clears jobs/packages/license inside
#       dev_releases/diego/diego-<ver>.yml (keeps the file) and prunes that version from
#       dev_releases/diego/index.yml so bosh can recreate it.
#       Set SKIP_DEV_RELEASE_PRUNE=1 to skip that step (will fail if version still exists).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Clear prior dev release state so bosh create-release --version=X can run again.
prune_dev_release_record() {
  local ver="$1"
  VERSION="$ver" ruby <<'RUBY'
require "yaml"
version = ENV.fetch("VERSION")
yml = "dev_releases/diego/diego-#{version}.yml"
if File.exist?(yml)
  data = YAML.load_file(yml)
  unless data.is_a?(Hash) && data["version"].to_s == version
    warn "Skipping #{yml}: unexpected or missing version field"
  else
    # BOSH treats the manifest as an existing dev release; strip regenerable sections only.
    %w[jobs packages license compiled_packages].each { |k| data.delete(k) }
    data.delete("commit_hash")
    data["uncommitted_changes"] = false if data.key?("uncommitted_changes")
    File.write(yml, YAML.dump(data))
    puts "Cleared jobs/packages/license (etc.) in #{yml}; file kept"
  end
end
idx = "dev_releases/diego/index.yml"
if File.exist?(idx)
  data = YAML.load_file(idx)
  if data.is_a?(Hash) && data["builds"].is_a?(Hash)
    before = data["builds"].size
    data["builds"] = data["builds"].reject do |_uuid, meta|
      meta.is_a?(Hash) && meta["version"].to_s == version
    end
    removed = before - data["builds"].size
    if removed.positive?
      File.write(idx, YAML.dump(data))
      puts "Pruned #{removed} entr#{removed == 1 ? 'y' : 'ies'} from #{idx} for version #{version}"
    end
  end
end
RUBY
}

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
  if [[ -z "${SKIP_DEV_RELEASE_PRUNE:-}" ]]; then
    echo "Pruning any existing dev release record for $VERSION (bosh refuses duplicate dev versions) ..."
    prune_dev_release_record "$VERSION"
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
