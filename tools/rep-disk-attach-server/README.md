# rep-disk-attach-server

HTTP server that runs on a jumpbox (or any host with BOSH and govc) and performs `govc vm.disk.attach` when Diego cell pre-start scripts call it during a rolling upgrade. This keeps vCenter and BOSH credentials off the cells.

## Prerequisites

- **bosh** CLI in PATH, logged in to the correct environment and deployment
- **govc** in PATH, with vCenter credentials in environment (GOVC_URL, GOVC_USERNAME, GOVC_PASSWORD, etc.)
- **jq** is not required; the server parses BOSH JSON in Go

## Build

```bash
cd tools/rep-disk-attach-server
go build -o rep-disk-attach-server .
```

## Configuration (environment)

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| PORT | No | 80 | Listen port |
| BOSH_DEPLOYMENT | Yes | - | BOSH deployment name (e.g. `cf`) |
| BOSH_INSTANCE_GROUP | No | diego_cell | Instance group name for cells (e.g. `diego_cell` or `compute`) |
| DATASTORE | Yes | - | vSphere datastore name (e.g. `iscsi-storage`). Disks are expected as `[DATASTORE] rep_cache_<N>.vmdk`. |
| AUTH_KEY | No | - | Pre-shared key; clients must send `X-Attach-Token: <AUTH_KEY>`. Omit to disable auth. |
| ATTACH_DEVICE_INFO_LOG | No | attach-device-info.log | Path to append `govc device.info` JSON for each attach (server-side). Set empty to disable. |
| ATTACH_HISTORY_FILE | No | - | Path to NDJSON attach history. When set, short-form requests (`rep_cache_N`) use the **last** `backing_vmdk` for that cell from this file instead of `[DATASTORE] rep_cache_N.vmdk`. |

vCenter: set **GOVC_URL**, **GOVC_USERNAME**, **GOVC_PASSWORD**, and optionally **GOVC_INSECURE**, **GOVC_DATACENTER**, etc., in the environment before starting the server.

## Run

```bash
export GOVC_URL='https://vcenter.example.com/sdk'
export GOVC_USERNAME=user
export GOVC_PASSWORD=pass
export GOVC_DATACENTER='<>'
export GOVC_DATASTORE='<>'
export GOVC_INSECURE=true
export DEPLOYMENT_NAME='cf-<>'
export BOSH_DEPLOYMENT_NAME='cf-<>'
export BOSH_INSTANCE_GROUP="<diego_cell/compute>"
export ATTACH_SERVER_AUTH_KEY='this-is-a-secret-token'
export PATH=$PATH:/usr/local/go/bin
export ATTACH_HISTORY_FILE=DISK_CREATE_ATTACH_HISTORY.txt
./rep-disk-attach-server
```

## API

- **POST /attach-disk/<disk-name-or-label>**  
  Example: `POST /attach-disk/rep_cache_0`  
  - Resolves instance `diego_cell/0` (or `BOSH_INSTANCE_GROUP/0`) to VM CID via `bosh instances --details --json`.  
  - **Short form** (`rep_cache_N`): if **ATTACH_HISTORY_FILE** is set, the server uses the **last** `backing_vmdk` for that cell (where `cell_instance` = `instanceGroup/N`) from the history file; otherwise uses `[DATASTORE] rep_cache_N.vmdk`.  
  - Runs `govc vm.disk.attach -vm=<vm_cid> -disk="<disk_path>"`.  
  - Returns **200** with `Content-Type: application/json`. The body is the output of `govc device.info -vm=<vm_cid> -json <device-name>` for the attached disk (or a fallback JSON if that fails).  
  - The same device-info JSON is appended to the file at **ATTACH_DEVICE_INFO_LOG** (default `attach-device-info.log`) on the server.  
  - If the disk is already attached, treated as success (idempotent).  
  - **Auth:** when `AUTH_KEY` is set, the request must include header `X-Attach-Token: <AUTH_KEY>`.

## Diego cell configuration

In the deployment manifest, set rep properties so the pre-start script calls this server:

```yaml
diego.rep.disk_attach_callback_url: "http://<jumpbox-dns-or-ip>"
diego.rep.disk_attach_callback_auth_key: "<same AUTH_KEY as server>"
```

If the server listens on a port other than 80, include it: `http://jumpbox:8080`.

The pre-start script will POST to `http://<url>/attach-disk/rep_cache_<index>` with body `{"index": <index>}` and header `X-Attach-Token` when the auth key is set, then wait up to 30s for the block device and mount it.

### Attach history file (short-form disk lookup)

When **ATTACH_HISTORY_FILE** is set, the server reads it to resolve short-form requests (`rep_cache_0`, `rep_cache_1`, …). The file must be NDJSON (one JSON object per line). Each line is used only if it has:

- **`cell_instance`**: `"compute/0"`, `"compute/1"`, etc. (must match `BOSH_INSTANCE_GROUP` + `/` + index).
- **`backing_vmdk`**: full datastore path, e.g. `"[iscsi-storage] compute_cf-xxx/compute_cf-xxx_32.vmdk"`.

The server uses the **last** line in the file that matches the requested cell (e.g. `compute/0`) and uses its `backing_vmdk` as the disk to attach. If the file is missing, unreadable, or has no matching line, the server falls back to `[DATASTORE] rep_cache_N.vmdk`. Append new attach results to the file so the next short-form request for that cell uses the latest backing.

## Capturing status code and response body on the compute (client)

To save the HTTP status code and response body locally on the cell/compute for reference:

```bash
# Directory to store attach responses (e.g. under /var/vcap/data or /tmp)
OUT_DIR="/var/vcap/data/rep/attach-callback"
mkdir -p "$OUT_DIR"

# Timestamp for this run
TS=$(date +%Y%m%d-%H%M%S)
RESPONSE_FILE="${OUT_DIR}/attach-response-${TS}.json"
STATUS_FILE="${OUT_DIR}/attach-status-${TS}.txt"

# Call attach endpoint; -w writes the HTTP code to stdout after the body; -o saves body to file
HTTP_CODE=$(curl -s -w "%{http_code}" -o "$RESPONSE_FILE" \
  -X POST \
  -H "X-Attach-Token: this-is-a-secret-token" \
  "http://<callback-server-ip>:8080/attach-disk/%5Biscsi-storage%5D%20compute_cf-daaa7d87cd6b48801a89_d3dfcd5d731b/compute_cf-daaa7d87cd6b48801a89_d3dfcd5d731b_20.vmdk")

echo "$HTTP_CODE" > "$STATUS_FILE"

# Optional: also append a one-line log for quick reference
echo "${TS} ${HTTP_CODE} ${RESPONSE_FILE}" >> "${OUT_DIR}/attach-callback.log"
```

- **Status code:** in `$HTTP_CODE` and in `$STATUS_FILE` (e.g. `200`).
- **Response body:** in `$RESPONSE_FILE` (JSON from the server, i.e. `govc device.info` output for the attached disk).
- Use `cat "$RESPONSE_FILE"` or `jq . "$RESPONSE_FILE"` to inspect the body later.

## Listing VMDK descendants (children, grandchildren, …)

To see all VMDKs that have a given base disk (e.g. `rep_cache_0.vmdk`) in their parent chain—i.e. children, grandchildren, etc.—use the helper script. It uses `govc datastore.ls` and reads each VMDK descriptor’s `parentFileNameHint` to build the tree.

**Requires:** `govc` with vCenter credentials (same as the attach server).

On large datastores the script runs many `govc datastore.download` calls; it uses **PARALLEL_JOBS** (default 8) concurrent downloads so 300+ descriptors finish in tens of seconds instead of minutes. Progress is printed to stderr. To speed it up further, limit the search to one VM folder with **SEARCH_PATH**:

```bash
export DATASTORE=iscsi-storage
./list-vmdk-descendants.sh rep_cache_0.vmdk
```

Faster (only scan one VM folder):

```bash
SEARCH_PATH=compute_cf-daaa7d87cd6b48801a89_d3dfcd5d731b DATASTORE=iscsi-storage ./list-vmdk-descendants.sh rep_cache_0.vmdk
```

Or with full path:

```bash
./list-vmdk-descendants.sh "[iscsi-storage] rep_cache_0.vmdk"
```

Output is a tree by level, e.g.:

```
Base VMDK: [iscsi-storage] rep_cache_0.vmdk
Descendants (children, grandchildren, ...):

  level 1: [iscsi-storage] compute_cf-xxx/compute_cf-xxx_20.vmdk
    level 2: [iscsi-storage] compute_cf-xxx/compute_cf-xxx_21.vmdk
```
