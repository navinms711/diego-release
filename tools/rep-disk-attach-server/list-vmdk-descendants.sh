#!/usr/bin/env bash
# List all children, grandchildren, etc. (descendants) of a given VMDK on a vSphere datastore.
# Uses govc to list VMDK descriptor files and parse parentFileNameHint to build the chain.
#
# Usage:
#   DATASTORE=iscsi-storage ./list-vmdk-descendants.sh rep_cache_0.vmdk
#   ./list-vmdk-descendants.sh "[iscsi-storage] rep_cache_0.vmdk"
#
# Optional: SEARCH_PATH limits listing to a subpath (faster on large datastores), e.g.:
#   SEARCH_PATH=compute_cf-daaa7d87cd6b48801a89_d3dfcd5d731b DATASTORE=iscsi-storage ./list-vmdk-descendants.sh rep_cache_0.vmdk
# Optional: PARALLEL_JOBS=8 (default 8) - number of parallel govc downloads when reading descriptors.
#
# Requires: govc

set -euo pipefail

DATASTORE="${DATASTORE:-}"
SEARCH_PATH="${SEARCH_PATH:-}"   # optional: only list under this path (e.g. one VM folder)
PARALLEL_JOBS="${PARALLEL_JOBS:-8}"  # parallel descriptor reads (default 8)
BASE_VMDK="${1:-}"

if [ -z "$BASE_VMDK" ]; then
  echo "Usage: DATASTORE=<name> $0 <base.vmdk>" >&2
  echo "Example: DATASTORE=iscsi-storage $0 rep_cache_0.vmdk" >&2
  exit 1
fi

# Normalize base: strip [datastore] prefix for internal use; we'll match by suffix or full path
BASE_NAME="${BASE_VMDK##*/}"
if [[ "$BASE_VMDK" == *"] "* ]]; then
  DATASTORE="${DATASTORE:-${BASE_VMDK%%]*}}"
  DATASTORE="${DATASTORE#[}"
  BASE_PATH="${BASE_VMDK#*] }"
else
  BASE_PATH="$BASE_VMDK"
fi

if [ -z "$DATASTORE" ]; then
  echo "Set DATASTORE (e.g. iscsi-storage) or pass full path [datastore] path/to/file.vmdk" >&2
  exit 1
fi

DS_PREFIX="[${DATASTORE}]"
LIST_PREFIX="${DS_PREFIX}"
[ -n "$SEARCH_PATH" ] && LIST_PREFIX="${DS_PREFIX}/${SEARCH_PATH}"
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT

# List all .vmdk files (descriptors only; exclude extent files -flat and -sesparse)
# Output: one path per line, relative to datastore root (e.g. vm_folder/file.vmdk)
list_vmdks() {
  local raw
  raw=$(govc datastore.ls -R "$LIST_PREFIX" 2>/dev/null || true \
    | sed 's|^'"${DS_PREFIX}"'/||' \
    | grep '\.vmdk$' \
    | grep -v '\-flat\.vmdk$' \
    | grep -v '\-sesparse\.vmdk$' \
    | grep -v '\-delta\.vmdk$')
  if [ -n "$SEARCH_PATH" ]; then
    echo "$raw" | sed 's|^|'"${SEARCH_PATH}"'/|'
  else
    echo "$raw"
  fi
}

# Resolve parent path: child_path + parentFileNameHint -> parent path relative to datastore
resolve_parent() {
  local child_path="$1"
  local hint="$2"
  local child_dir
  child_dir=$(dirname "$child_path")
  if [ "$child_dir" = "." ]; then
    echo "$hint"
  else
    # Normalize: replace ".." in hint if present
    echo "${child_dir}/${hint}" | sed 's|/\.\./|/|g; s|^\.\./||'
  fi
}

# Get parentFileNameHint from a VMDK descriptor (govc datastore.download to stdout)
get_parent_hint() {
  local path="$1"
  govc datastore.download "${DS_PREFIX}/${path}" - 2>/dev/null \
    | grep -i 'parentFileNameHint' \
    | sed 's/.*="\(.*\)".*/\1/' \
    | sed 's/"//g' \
    | tr -d '\r'
}

# Worker: read one descriptor, output "parent_path|child_path" or nothing. Used for parallel runs.
process_one_descriptor() {
  local child_path="$1"
  local hint
  hint=$(govc datastore.download "${DS_PREFIX}/${child_path}" - 2>/dev/null \
    | grep -i 'parentFileNameHint' \
    | sed 's/.*="\(.*\)".*/\1/' \
    | sed 's/"//g' \
    | tr -d '\r')
  [ -z "$hint" ] && return 0
  local parent_path
  parent_path=$(resolve_parent "$child_path" "$hint")
  echo "${parent_path}|${child_path}"
}
export -f process_one_descriptor 2>/dev/null || true
export DS_PREFIX

# Build parent -> children map (one line per child: "parent_path|child_path")
# Uses PARALLEL_JOBS concurrent descriptor reads to avoid being stuck on 300+ govc calls.
build_map() {
  local max_jobs=$((PARALLEL_JOBS))
  [ "$max_jobs" -lt 1 ] && max_jobs=1
  local running=0
  while IFS= read -r child_path; do
    [ -z "$child_path" ] && continue
    process_one_descriptor "$child_path" >> "$MAP_FILE" &
    running=$((running + 1))
    while [ "$running" -ge "$max_jobs" ]; do
      wait -n 2>/dev/null || wait
      running=$((running - 1))
    done
  done
  wait 2>/dev/null || true
}

# Match base: exact path, or path ending with /base name, or bare base name
base_matches() {
  local path="$1"
  [[ "$path" == "$BASE_PATH" ]] || [[ "$path" == *"/${BASE_NAME}" ]] || [[ "$path" == "$BASE_NAME" ]]
}

# Collect all parent|child lines
MAP_FILE="$TEMP_DIR/parent_child.txt"
VMDK_LIST_FILE="$TEMP_DIR/vmdk_list.txt"

echo "Listing VMDK files on ${LIST_PREFIX} (this can be slow on large datastores)..." >&2
list_vmdks > "$VMDK_LIST_FILE"
VMDK_COUNT=$(wc -l < "$VMDK_LIST_FILE" | tr -d ' ')
echo "Found $VMDK_COUNT VMDK descriptor(s). Reading parent hints (${PARALLEL_JOBS} parallel)..." >&2
: > "$MAP_FILE"
< "$VMDK_LIST_FILE" build_map

declare -A PARENT_OF
while IFS='|' read -r parent child; do
  [ -z "$child" ] && continue
  PARENT_OF[$child]="$parent"
done < "$MAP_FILE"

# Get children of a parent path (exact match on first column)
children_of() {
  local parent="$1"
  awk -v p="$parent" -F'|' '$1==p {print $2}' "$MAP_FILE" 2>/dev/null
}

# BFS from base: start with all VMDKs whose parent matches the base
QUEUE=()
for c in "${!PARENT_OF[@]}"; do
  parent="${PARENT_OF[$c]}"
  if base_matches "$parent"; then
    QUEUE+=("$c")
  fi
done

declare -A VISITED

echo "Base VMDK: ${DS_PREFIX}/${BASE_PATH}"
echo "Descendants (children, grandchildren, ...):"
echo ""

if [ ${#QUEUE[@]} -eq 0 ]; then
  echo "  (none found on this datastore)"
  exit 0
fi

LEVEL=0
while [ ${#QUEUE[@]} -gt 0 ]; do
  LEVEL=$((LEVEL + 1))
  CURRENT=("${QUEUE[@]}")
  QUEUE=()
  for node in "${CURRENT[@]}"; do
    [ -n "${VISITED[$node]:-}" ] && continue
    VISITED[$node]=1
    indent=$(printf '%*s' $((LEVEL * 2)) '')
    echo "  ${indent}level $LEVEL: ${DS_PREFIX}/${node}"
    while IFS= read -r kid; do
      [ -z "$kid" ] && continue
      [ -z "${VISITED[$kid]:-}" ] && QUEUE+=("$kid")
    done < <(children_of "$node")
  done
done
