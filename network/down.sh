#!/usr/bin/env bash
# Tear down the DRUNIX test network and remove the artefacts it generated.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

configure_platform

if [ ! -f "${TEST_NETWORK_DIR}/network.sh" ]; then
  warn "upstream test network not present at ${TEST_NETWORK_DIR}; nothing to tear down"
  exit 0
fi

log "tearing down the network"
cd "${TEST_NETWORK_DIR}"
./network.sh down

log "done. Remaining containers:"
docker ps --format 'table {{.Names}}\t{{.Status}}' | head -10
