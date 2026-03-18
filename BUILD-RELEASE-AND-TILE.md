# Build Diego 2.128.0+dev.14 release and 10.2.7 tile

Use this to build a Diego BOSH release and a custom TAS 10.2.7 tile that includes it (cache on store, `/var/vcap/store/rep_download_cache` when pre-warmed disk is mounted, disk-attach callback support).

## Scripts

- **build-diego-dev14.sh** – Builds the Diego release tarball `diego-2.128.0+dev.14.tgz` (run from `diego-release/`).
- **update-tile-to-dev14.sh** – Builds a 10.2.7 tile that uses that tarball. Uses `../upgrade-6.0.9-to-10.2.7/metadata-10.2.7-dev14.yml` and replaces the Diego release inside a base tile; it also replaces the placeholder `REPLACE_ME_DEV14` in metadata with the actual SHA1 of the tarball.

Both scripts are intended to be run as-is. No manual hash/filename/version edits are needed for the tile: the script computes the tarball SHA1 and updates metadata.

## 1. Blobstore access (required for release build)

After merging upstream, the release references blobs (e.g. `auctioneer`, `file_server`) that are stored in the Cloud Foundry blobstore. They must be downloaded before `bosh create-release` can build the tarball.

- Ensure **config/private.yml** exists and is configured for your blobstore (e.g. S3). See BOSH/docs for the format. Without it, `bosh sync-blobs` cannot download those blobs.
- Then run:

  ```bash
  cd /home/pivotal/workspace/diego-release
  bosh sync-blobs
  ```

If you see errors like “Cannot find blob named 'auctioneer/...'” when building, the missing blobs were not synced (usually due to missing or wrong `config/private.yml`).

## 2. Build the Diego release

```bash
cd /home/pivotal/workspace/diego-release
./build-diego-dev14.sh
```

This produces `diego-2.128.0+dev.14.tgz` in the same directory.

## 3. Build the 10.2.7 tile

Use your base 10.2.7 tile (e.g. the one at `/tmp/cf-10.2.7-build.2.pivotal`):

```bash
./update-tile-to-dev14.sh /tmp/cf-10.2.7-build.2.pivotal /home/pivotal/workspace/cf-10.2.7-cache-on-store.pivotal
```

Or with defaults (input: `/tmp/cf-10.2.7-build.2.pivotal`, output: `../cf-10.2.7-cache-on-store.pivotal`):

```bash
./update-tile-to-dev14.sh
```

The script will:

- Replace the Diego release inside the tile with `diego-2.128.0+dev.14.tgz`.
- Use metadata from `../upgrade-6.0.9-to-10.2.7/metadata-10.2.7-dev14.yml` and substitute the tarball’s SHA1 for `REPLACE_ME_DEV14`.

No manual metadata edits are required.

## 4. 6.x → 10.2.7 upgrade and Rep cache

- On your 6.x testbed, add the 10.2.7 tile (this custom tile) and run the upgrade as usual (e.g. Apply changes). BOSH will use the new Diego release (2.128.0+dev.14) from the tile.
- To have Rep use the pre-warmed droplet cache at **/var/vcap/store/rep_download_cache**:
  - Use the **disk attach callback** (recommended): configure **Disk attach callback URL** (and optional auth key) in the tile/Rep job so the pre-start script can call your attach server. Pre-start will then attach the disk, wait for the block device, and mount it at `/var/vcap/store/rep_download_cache`. Rep prefers this path when it is a mount point; otherwise it uses the ephemeral path `/var/vcap/data/rep/shared/garden/download_cache`.
  - Or attach the cache disk by some other means; Rep still prefers `/var/vcap/store/rep_download_cache` when it is a mount point.
- After upgrade, Rep logs the resolved cache path at startup (e.g. `rep-config-download-cache` with `cache_path`). Check rep logs to confirm it is using `/var/vcap/store/rep_download_cache` when the disk is mounted.

## Verify tile

```bash
unzip -l cf-10.2.7-cache-on-store.pivotal | grep diego
unzip -p cf-10.2.7-cache-on-store.pivotal metadata/metadata.yml | grep -A4 'name: diego'
```

You should see `diego-2.128.0+dev.14.tgz` and version `2.128.0+dev.14` with a valid `sha1`.
