#!/usr/bin/env bash
set -euo pipefail

# --- Configuration ---
DEPLOYMENT_NAME="${DEPLOYMENT_NAME:-cf}"
DATASTORE="${GOVC_DATASTORE:-datastore1}"
DISK_SIZE_GB="${DISK_SIZE_GB:-20}"
CACHE_LABEL="REP_CACHE"
MASTER_RECORD="DISK_CREATE_ATTACH_HISTORY.txt"
# Max concurrent jobs per phase (attach, rsync, detach). Keep low (4–8) to avoid overloading BOSH/vCenter.
PARALLEL_JOBS="${PARALLEL_JOBS:-4}"

# --- 1) Get number of cells ---
CELL_COUNT=$(bosh -d "$DEPLOYMENT_NAME" instances --json | \
  jq -r '.Tables[0].Rows[] | select(.instance | startswith("compute/")) | .instance' | wc -l)
[[ "$CELL_COUNT" -gt 0 ]] || { echo "No compute instances found"; exit 1; }
echo "Found $CELL_COUNT compute cells; running up to $PARALLEL_JOBS in parallel."

TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT

# --- 2) Create N VMDKs (parallel, limited) ---
create_one() {
  local i="$1"
  local DISK_NAME="rep_cache_$((i-1))"
  if ! govc datastore.ls "[${DATASTORE}]" 2>/dev/null | grep -q "^${DISK_NAME}\.vmdk$"; then
    govc datastore.disk.create -ds="$DATASTORE" -size="${DISK_SIZE_GB}G" "${DISK_NAME}.vmdk"
  fi
  echo "Created or found VMDK: ${DISK_NAME}"
}
export -f create_one
export DATASTORE DISK_SIZE_GB
for i in $(seq 1 "$CELL_COUNT"); do
  while [ "$(jobs -r | wc -l)" -ge "$PARALLEL_JOBS" ]; do sleep 1; done
  create_one "$i" &
done
wait
echo "Step 2 (create disks) done."

# --- 3) Attach disk per cell, write one JSON line to temp file (parallel) ---
attach_one() {
  local i="$1"
  local idx=$((i-1))
  local CELL_INSTANCE="compute/${idx}"
  local DISK_NAME="rep_cache_${idx}"
  local outfile="$TEMP_DIR/history_${idx}.txt"
  VM_CID=$(bosh -d "$DEPLOYMENT_NAME" instances --details --json | jq -r --arg idx "$idx" '.Tables[0].Rows[] | select((.instance | startswith("compute/")) and ((.index | tostring) == $idx)) | .vm_cid')
  [[ -n "$VM_CID" ]] || { echo "No VM CID for $CELL_INSTANCE"; return 1; }
  govc vm.disk.attach -vm="$VM_CID" -disk="[${DATASTORE}] ${DISK_NAME}.vmdk" || true
  sleep 1
  DEVICE_NAME=$(govc device.info -vm="${VM_CID}" -json | jq -r --arg diskname "${DISK_NAME}" '.devices[] | select(.type == "VirtualDisk" and ( ((.backing.fileName // "") | contains($diskname)) or ((.backing.parent.fileName // "") | contains($diskname)) )) | .name')
  BACKING_FILENAME=$(govc device.info -vm="${VM_CID}" -json "${DEVICE_NAME}" | jq -r .devices[].backing.fileName)
  TS=$(date +%Y%m%d-%H%M%S)
  echo "{\"timestamp\": \"${TS}\", \"bosh_deployment\": \"${DEPLOYMENT_NAME}\", \"cell_instance\": \"${CELL_INSTANCE}\", \"vm_cid\": \"${VM_CID}\", \"disk_name\": \"${DISK_NAME}.vmdk\", \"block_device\": \"${DEVICE_NAME}\", \"backing_vmdk\": \"${BACKING_FILENAME}\"}" > "$outfile"
  echo "Attached $DISK_NAME to $VM_CID"
}
export -f attach_one
export DEPLOYMENT_NAME DATASTORE TEMP_DIR
for i in $(seq 1 "$CELL_COUNT"); do
  while [ "$(jobs -r | wc -l)" -ge "$PARALLEL_JOBS" ]; do sleep 1; done
  attach_one "$i" &
done
wait
for i in $(seq 0 $((CELL_COUNT-1))); do
  [ -f "$TEMP_DIR/history_${i}.txt" ] && cat "$TEMP_DIR/history_${i}.txt" >> "$MASTER_RECORD"
done
echo "Step 3 (attach) done."

# --- 4) Rsync on each cell (parallel bosh ssh) ---
rsync_one() {
  local i="$1"
  local idx=$((i-1))
  local CELL_INSTANCE="compute/${idx}"
  bosh -d "$DEPLOYMENT_NAME" ssh "$CELL_INSTANCE" --command='sudo bash -c "
    set -e
    MOUNT_PATH=/var/vcap/store/rep_download_cache
    CACHE_LABEL=REP_CACHE
    CACHE_SRC=/var/vcap/data/rep/shared/garden/download_cache
    for h in /sys/class/scsi_host/host*/scan; do echo \"- - -\" > \"\$h\" 2>/dev/null || true; done
    sleep 2
    DEV=
    for b in /dev/sd[a-z] /dev/sd[a-z][a-z] /dev/xvd[a-z]; do
      [ -b \"\$b\" ] || continue
      LABEL=\$(blkid -o value -s LABEL \"\$b\" 2>/dev/null || true)
      if [ \"\$LABEL\" = \"\$CACHE_LABEL\" ]; then DEV=\$b; break; fi
    done
    if [ -z \"\$DEV\" ]; then
      for b in /dev/sd[a-z] /dev/sd[a-z][a-z] /dev/xvd[a-z]; do
        [ -b \"\$b\" ] || continue
        grep -qE \"^\${b}([0-9]*)? \" /proc/mounts && continue
        LABEL=\$(blkid -o value -s LABEL \"\$b\" 2>/dev/null || true)
        [ -z \"\$LABEL\" ] || continue
        DEV=\$b; break
      done
    fi
    [ -n \"\$DEV\" ] || { echo No suitable block device; exit 1; }
    if [ \"\$(blkid -o value -s LABEL \"\$DEV\" 2>/dev/null)\" != \"\$CACHE_LABEL\" ]; then
      mkfs.ext4 -L \"\$CACHE_LABEL\" \"\$DEV\" || true
    fi
    mkdir -p /var/vcap/store
    mkdir -p \"\$MOUNT_PATH\"
    mount -L \"\$CACHE_LABEL\" \"\$MOUNT_PATH\" || true
    rsync -ac --delete \"\${CACHE_SRC}/\" \"\$MOUNT_PATH\" || true
    du -sb \"\${CACHE_SRC}/\" \"\$MOUNT_PATH\" || true
    umount -l \"\$MOUNT_PATH\"
    sync
    blockdev --flushbufs \"\$DEV\" 2>/dev/null || true
    sleep 5
    echo Done.
  "'
  echo "Rsync done for $CELL_INSTANCE"
}
export -f rsync_one
export DEPLOYMENT_NAME
for i in $(seq 1 "$CELL_COUNT"); do
  while [ "$(jobs -r | wc -l)" -ge "$PARALLEL_JOBS" ]; do sleep 1; done
  rsync_one "$i" &
done
wait
echo "Step 4 (rsync) done."

# --- 5) Print backing VMDK per cell (parallel) ---
backing_one() {
  local idx="$1"
  local VM_CID DISK_NAME DEVICE_NAME BACKING_FILENAME
  VM_CID=$(bosh -d "$DEPLOYMENT_NAME" instances --details --json | jq -r --arg idx "$idx" '.Tables[0].Rows[] | select((.instance | startswith("compute/")) and ((.index | tostring) == $idx)) | .vm_cid')
  DISK_NAME="rep_cache_${idx}"
  DEVICE_NAME=$(govc device.info -vm="${VM_CID}" -json | jq -r --arg diskname "$DISK_NAME" '.devices[] | select(.type == "VirtualDisk" and ( ((.backing.fileName // "") | contains($diskname)) or ((.backing.parent.fileName // "") | contains($diskname)) )) | .name')
  BACKING_FILENAME=$(govc device.info -vm="${VM_CID}" -json "${DEVICE_NAME}" | jq -r .devices[].backing.fileName)
  echo "On $VM_CID: Use this Backing file: $BACKING_FILENAME for the next govc vm.disk.attach .."
}
export -f backing_one
export DEPLOYMENT_NAME
for i in $(seq 0 $((CELL_COUNT-1))); do
  while [ "$(jobs -r | wc -l)" -ge "$PARALLEL_JOBS" ]; do sleep 1; done
  backing_one "$i" &
done
wait
echo "Step 5 done."

# --- 6) Detach disk per cell (parallel) ---
detach_one() {
  local idx="$1"
  local VM_CID DISK_NAME DISK_CANONICAL_NAME
  VM_CID=$(bosh -d "$DEPLOYMENT_NAME" instances --details --json | jq -r --arg idx "$idx" '.Tables[0].Rows[] | select((.instance | startswith("compute/")) and ((.index | tostring) == $idx)) | .vm_cid')
  DISK_NAME="rep_cache_${idx}"
  DISK_CANONICAL_NAME=$(govc device.info -vm="$VM_CID" -json | jq -r --arg name "$DISK_NAME" '.devices[] | select(.type == "VirtualDisk" and ((.backing.parent.fileName // "") | contains($name))) | .name')
  govc device.remove -vm="$VM_CID" -keep $DISK_CANONICAL_NAME
  echo "Detached from $VM_CID"
}
export -f detach_one
export DEPLOYMENT_NAME
for i in $(seq 0 $((CELL_COUNT-1))); do
  while [ "$(jobs -r | wc -l)" -ge "$PARALLEL_JOBS" ]; do sleep 1; done
  detach_one "$i" &
done
wait
echo "Step 6 (detach) done."
