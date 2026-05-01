#!/usr/bin/env bash
# Copy missing /opsmgr/cf-<dest>/... credentials from a sibling /opsmgr/cf-<src>/... branch
# in CredHub (same types/values). Use when BOSH render fails with 404 on otel paths but an
# older CF product id still has the certs.
#
# Prerequisites (on Ops Manager or jumpbox with CredHub access to the foundation CredHub):
#   credhub api --server=... --ca-cert=...
#   credhub login ...   # so `credhub get` works for the SOURCE paths
#
# Usage:
#   export SOURCE_PREFIX=/opsmgr/cf-c91efccda41b0840dbbc
#   export DEST_PREFIX=/opsmgr/cf-611112fb89886934bce1
#   ./scripts/copy-opsmgr-cf-credhub-branch.sh
#
# Requires: credhub CLI, jq.

set -euo pipefail

SOURCE_PREFIX="${SOURCE_PREFIX:-/opsmgr/cf-c91efccda41b0840dbbc}"
DEST_PREFIX="${DEST_PREFIX:-/opsmgr/cf-611112fb89886934bce1}"

CREDS=(
  otel_collector_tls_cert
  cf_hub_collector_tls_cert
)

for leaf in "${CREDS[@]}"; do
  src="${SOURCE_PREFIX}/${leaf}"
  dst="${DEST_PREFIX}/${leaf}"
  echo "==> $src -> $dst"
  if credhub get -n "$dst" >/dev/null 2>&1; then
    echo "    destination already exists; skip"
    continue
  fi
  tmp="$(mktemp)"
  credhub get -n "$src" -j | jq --arg dst "$dst" '{credentials: [{name: $dst, type: .type, value: .value}]}' >"$tmp"
  credhub import -j -f "$tmp"
  rm -f "$tmp"
  echo "    imported OK"
done

echo "Done. Verify:"
echo "  credhub get -n ${DEST_PREFIX}/otel_collector_tls_cert"
echo "Then re-run Apply Changes / bosh deploy."
