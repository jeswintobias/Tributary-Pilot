#!/usr/bin/env bash
# Prove threat T2.1 on the live network: an organisation that is not a party to
# a transfer cannot move it, even though it is a full channel member.
#
# Until PlatformOrg existed, T2.1 was only covered against a mock stub, because
# the two organisations on the network were both parties to every transfer.
# PlatformOrg is a channel member with a valid admin identity and Writer rights,
# so its proposals reach the chaincode — and the chaincode's own access control
# is what must turn them away.
#
# Run after: ./network/deploy-cc.sh settlement && ./network/add-platform-org.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
COMMIT_WAIT="${COMMIT_WAIT:-8}"
LITE_PEER=0
STAMP="$(date +%s)"

ORG3_HOME="${TRIBUTARY_HOME}/platformorg"
ORG3_ADMIN_MSP="${ORG3_HOME}/organizations/peerOrganizations/org3.example.com/users/Admin@org3.example.com/msp"

configure_platform
require_tools
ensure_upstream
export_fabric_env

cd "${TEST_NETWORK_DIR}"
set +u
# shellcheck disable=SC1091
source ./scripts/envVar.sh

[ -d "${ORG3_ADMIN_MSP}" ] || die "PlatformOrg identity not found — run ./network/add-platform-org.sh first"

ORDERER_CA="${TEST_NETWORK_DIR}/organizations/ordererOrganizations/example.com/tlsca/tlsca.example.com-cert.pem"
PEER0_ORG1_CA="${TEST_NETWORK_DIR}/organizations/peerOrganizations/org1.example.com/tlsca/tlsca.org1.example.com-cert.pem"
PEER0_ORG2_CA="${TEST_NETWORK_DIR}/organizations/peerOrganizations/org2.example.com/tlsca/tlsca.org2.example.com-cert.pem"

BOTH_PEERS=(
  --peerAddresses localhost:7051 --tlsRootCertFiles "${PEER0_ORG1_CA}"
  --peerAddresses localhost:9051 --tlsRootCertFiles "${PEER0_ORG2_CA}"
)

step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
pass() { printf '\033[1;32mOK: %s\033[0m\n' "$*"; }

invoke() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}"
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" "${BOTH_PEERS[@]}" -c "$1"
  sleep "${COMMIT_WAIT}"
}

query_raw() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

# Point the CLI at PlatformOrg's admin identity. setGlobals has no case for
# org 3 — PlatformOrg runs no peer — so the environment is set here instead,
# addressing Org1's lite peer with Org3's signing material.
setPlatformOrg() {
  export CORE_PEER_LOCALMSPID="Org3MSP"
  export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
  export CORE_PEER_ADDRESS="localhost:7051"
  export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
  export CORE_PEER_TLS_ENABLED=true
}

# expect_rejected <description> <json-args> [expected-message]
# Submits as PlatformOrg and requires the call to be refused by the chaincode
# with a specific message. A refusal for any other reason is a failure: we are
# testing the contract's access control, not the channel ACL.
expect_rejected() {
  local what="$1" args="$2" want="${3:-is not a party to transfer}" out rc
  setPlatformOrg
  set +e
  out="$(peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" "${BOTH_PEERS[@]}" -c "${args}" 2>&1)"
  rc=$?
  set -e

  if [ ${rc} -eq 0 ]; then
    printf '%s\n' "${out}" >&2
    die "${what}: PlatformOrg was ALLOWED to do this"
  fi
  if printf '%s' "${out}" | grep -q "${want}"; then
    pass "${what} — rejected by the chaincode: \"${want}\""
  else
    printf '%s\n' "${out}" >&2
    die "${what}: rejected, but not with \"${want}\" — see output above"
  fi
}

# ---------------------------------------------------------------------------
step "Setting up a transfer between SenderOrg and PayoutBankOrg"
# ---------------------------------------------------------------------------
TID="TP-T21-${STAMP}"
PAYEE="sha256:collegefeespayeeref0000000000000000"
EXPIRY=$(( $(date +%s) + 900 ))

invoke 1 '{"Args":["SeedBalance","AED","100000000"]}'
invoke 2 '{"Args":["SeedBalance","INR","1000000000"]}'
invoke 1 "{\"Args\":[\"CreateTransfer\",\"${TID}\",\"220000\",\"AED\",\"3000000\",\"INR\",\"quote-${STAMP}\",\"${EXPIRY}\",\"Org2MSP\",\"${PAYEE}\"]}"
invoke 2 "{\"Args\":[\"Accept\",\"${TID}\"]}"
invoke 1 "{\"Args\":[\"Prepare\",\"${TID}\"]}"

STATE="$(query_raw 1 "{\"Args\":[\"GetTransfer\",\"${TID}\"]}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')"
[ "${STATE}" = "PREPARED" ] || die "expected PREPARED before the test, got ${STATE}"
log "transfer ${TID} is PREPARED, with both legs locked"

# ---------------------------------------------------------------------------
step "T2.1 — PlatformOrg is a channel member, but not a party to this transfer"
# ---------------------------------------------------------------------------
expect_rejected "Commit"  "{\"Args\":[\"Commit\",\"${TID}\"]}"
expect_rejected "Abort"   "{\"Args\":[\"Abort\",\"${TID}\"]}"
expect_rejected "Prepare" "{\"Args\":[\"Prepare\",\"${TID}\"]}"
expect_rejected "Accept"  "{\"Args\":[\"Accept\",\"${TID}\"]}" "only the payout org"

# ---------------------------------------------------------------------------
step "PlatformOrg CAN read — it is a regulator observer, after all"
# ---------------------------------------------------------------------------
setPlatformOrg
OBSERVED="$(peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
  -c "{\"Args\":[\"GetTransfer\",\"${TID}\"]}" 2>/dev/null)"
printf '%s\n' "${OBSERVED}"
printf '%s' "${OBSERVED}" | grep -q '"state":"PREPARED"' \
  || die "PlatformOrg could not read the transfer it is supposed to observe"
pass "PlatformOrg read the transfer and sees it unchanged at PREPARED"

# ---------------------------------------------------------------------------
step "The parties themselves can still settle it"
# ---------------------------------------------------------------------------
invoke 2 "{\"Args\":[\"Commit\",\"${TID}\"]}"
FINAL="$(query_raw 1 "{\"Args\":[\"GetTransfer\",\"${TID}\"]}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')"
[ "${FINAL}" = "SETTLED" ] || die "expected SETTLED after a legitimate commit, got ${FINAL}"
pass "SenderOrg and PayoutBankOrg settled it normally — state SETTLED"

printf '\n\033[1;32m── T2.1 verified on the live network\033[0m\n'
