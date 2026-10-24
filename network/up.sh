#!/usr/bin/env bash
# Bring up the DRUNIX test network and create the Tributary Pilot channel.
#
# Org1 is SenderOrg (UAE money transfer operator).
# Org2 is PayoutBankOrg (Indian payout bank).
# PlatformOrg is not stood up in Phase 0 — see docs/ARCHITECTURE.md section 3.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

configure_platform
require_tools
ensure_upstream
ensure_binaries
export_fabric_env

log "bringing up the network and creating channel '${CHANNEL_NAME}'"
cd "${TEST_NETWORK_DIR}"
./network.sh up createChannel -c "${CHANNEL_NAME}" "$@"

log "network is up. Containers:"
docker ps --format 'table {{.Names}}\t{{.Status}}' | head -20

cat <<EOF

Next:
  ${REPO_DIR}/network/deploy-cc.sh settlement
  ${REPO_DIR}/network/demo.sh
EOF
