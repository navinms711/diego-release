# Droplet cache on persistent disk (/var/vcap/store)

The rep's download cache (used for droplet locality) is now configured to use the **persistent disk** so cached droplets survive VM recreates.

## Building the Diego release (same as 6.0.9 / droplet locality)

The **same method** is used as when you built the Diego release for the 6.0.9 droplet-locality tile (e.g. `diego-2.128.0+dev.13.tgz`):

- **Command:** From the `diego-release` directory run:
  ```bash
  bosh create-release --version 2.128.0+cache-on-store.1 --tarball=diego-2.128.0+cache-on-store.1.tgz --force
  ```
- **Why it worked for dev.13 but failed for cache-on-store:** In the environment where you built the 10.2.7/dev.13 release, **blobs were already available** (from a previous `bosh sync-blobs` or from blobstore access). In the environment where we tried the cache-on-store build, blobs were not present, so BOSH reported "Cannot find blob". There is no different "way" to build—only blob availability differs.

**What you need to build locally (no single “build file”):**

1. **Repo:** This `diego-release` directory with the cache-on-store changes.
2. **Blobs:** One of the following:
   - **Same environment:** Run the same `bosh create-release` command from the **same machine/setup** where you successfully built `diego-2.128.0+dev.13.tgz` (blobs are already there), or
   - **Blobstore access:** Create **`config/private.yml`** in diego-release with your blobstore credentials so BOSH can download blobs. The release uses S3 (`config/final.yml` has `provider: s3`, `bucket_name: diego-release-blobs`). Example (fill in real values):
     ```yaml
     ---
     blobstore:
       options:
         access_key_id: YOUR_ACCESS_KEY
         secret_access_key: YOUR_SECRET_KEY
     ```
     Then run **`bosh sync-blobs`** from `diego-release` to fetch blobs, then run `bosh create-release` as above.
   - **Optional – local blobstore:** To build without S3 after a one-time sync, you can use a local blobstore by adding to `config/private.yml`: `blobstore: { provider: local, options: { blobstore_path: /path/to/local/blobs } }`. Sync once with S3 credentials, then you can build offline.

## Changes (diego-release)

| File | Change |
|------|--------|
| `jobs/rep/templates/rep.json.erb` | `download_cache_dir` set to `/var/vcap/store/rep/download_cache` (was `/var/vcap/data/rep/shared/garden/download_cache`). |
| `jobs/rep/templates/setup_mounted_data_dirs.erb` | Creates `/var/vcap/store/rep/download_cache` and sets ownership to `vcap:vcap`. |
| `jobs/rep/templates/bpm.yml.erb` | Added writable volume `/var/vcap/store/rep` so the rep process can write to the cache. |

## Requirements

- Diego cells must have a **persistent disk** mounted at `/var/vcap/store` (e.g. the 10GB disk you attached). The rep will use `/var/vcap/store/rep/download_cache` for the droplet cache.

## Version and manifest

- **Release version to use:** `2.128.0+cache-on-store.1` (or `2.129.0+cache-on-store.1` if you prefer).
- **In your BOSH manifest** (or Ops Manager stemcell/release config), set the Diego release version to this value after uploading the tarball.

## Deploying the change

This is a **Diego BOSH release** change. You do **not** need to change the TAS (CF) tile metadata for this; you need to build and use a new Diego release.

### Option A: Build and upload Diego release (recommended)

1. Ensure blobs are available: from an environment with blobstore access, run `bosh sync-blobs` (and have `config/private.yml` with S3 credentials if required). Without this, `bosh create-release` will fail with "Cannot find blob".
2. From this diego-release repo (with the cache-on-store changes):
   ```bash
   cd /home/pivotal/workspace/diego-release
   bosh create-release --version 2.128.0+cache-on-store.1 --tarball=diego-2.128.0+cache-on-store.1.tgz --force
   ```
3. Upload the release to your BOSH director:
   ```bash
   bosh upload-release diego-2.128.0+cache-on-store.1.tgz
   ```
4. Update your deployment to use Diego version **`2.128.0+cache-on-store.1`** (manifest or Ops Manager).
5. Redeploy the Diego Cell job so the new rep template is applied. The rep will then use `/var/vcap/store/rep/download_cache` for the cache.

### Option B: Custom 6.0.9 tile that includes this Diego release

If you want a single TAS 6.0.9 tile that contains this Diego release:

1. Build the Diego release tarball (as in Option A).
2. Start from your stock 6.0.9 tile (e.g. `cf-6.0.9-build.2.pivotal`). Extract it, replace the `releases/diego-*.tgz` with your new tarball, update `metadata/metadata.yml` (diego release `file`, `version`, `sha1`), and re-zip the tile.
3. Upload that custom tile to Ops Manager and use it for the foundation.

The tile-6.0.9-edit folder in the workspace only changes the **TAS tile metadata** (e.g. Diego Cell persistent disk configurable); it does not replace the Diego release tarball. To include this cache-on-store change in a tile, you must replace the Diego release inside the tile with the tarball from step 1.

## Summary

- **Cache path:** `/var/vcap/store/rep/download_cache`
- **Location of code changes:** `diego-release/jobs/rep/templates/` (rep.json.erb, setup_mounted_data_dirs.erb, bpm.yml.erb)
- **New tile:** Optional; building and uploading a new Diego release is sufficient for the cache to use the persistent disk.
