#!/usr/bin/env bash
# Package, install, approve and commit one of our chaincodes.
#
#   ./network/deploy-cc.sh settlement
#
# The endorsement policy is the point of this script. Commit must be endorsed by
# BOTH SenderOrg and PayoutBankOrg, so that neither can settle a transfer the
# other has not agreed to. Fabric applies a chaincode-level policy to every
# function, not just Commit; per-function policy needs state-based endorsement
# and is Phase 1 work. The stricter policy is the safe direction to err in.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${1:-settlement}"
CC_VERSION="${CC_VERSION:-1.0}"
CC_POLICY="${CC_POLICY:-AND('${SENDER_ORG_MSP}.peer','${PAYOUT_ORG_MSP}.peer')}"

CC_SRC="${REPO_DIR}/chaincode/${CC_NAME}"
[ -d "${CC_SRC}" ] || die "no chaincode at ${CC_SRC}"
[ -f "${CC_SRC}/go.mod" ] || die "no go.mod at ${CC_SRC} — is this chaincode implemented yet?"

configure_platform
require_tools
ensure_upstream
ensure_binaries
ensure_cc_builder_images
export_fabric_env

# Upstream's deployCC does not survive a source path containing a space, and our
# repository may well sit in one. Stage a copy at a clean path and deploy that.
CC_STAGE="${TRIBUTARY_HOME}/cc-stage/${CC_NAME}"
assert_no_spaces "${CC_STAGE}" "the staging path"
log "staging ${CC_NAME} chaincode at ${CC_STAGE}"
rm -rf "${CC_STAGE}"
mkdir -p "$(dirname "${CC_STAGE}")"
cp -R "${CC_SRC}" "${CC_STAGE}"

# Vendor dependencies into the staged copy. The chaincode builder container has
# no network access, so the module cache has to travel with the source.
log "vendoring chaincode dependencies"
( cd "${CC_STAGE}" && go mod tidy && go mod vendor )

log "deploying ${CC_NAME} v${CC_VERSION} to channel ${CHANNEL_NAME}"
log "endorsement policy: ${CC_POLICY}"
cd "${TEST_NETWORK_DIR}"
./network.sh deployCC \
  -c "${CHANNEL_NAME}" \
  -ccn "${CC_NAME}" \
  -ccp "${CC_STAGE}" \
  -ccl go \
  -ccv "${CC_VERSION}" \
  -ccep "${CC_POLICY}"

log "${CC_NAME} deployed with policy ${CC_POLICY}"
