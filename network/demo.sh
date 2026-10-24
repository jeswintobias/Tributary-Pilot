#!/usr/bin/env bash
# Run the happy path and the abort path against the live network and print the
# resulting states.
#
# This is the script that decides whether the README may claim the settlement
# chaincode works on DRUNIX, as opposed to working against a mock stub. It also
# tries to commit with a single organisation's endorsement, which must fail.
#
# Run it on a FRESH network. Balances accumulate across runs, and a run that was
# interrupted mid-way leaves a PREPARED transfer whose locks are only released at
# its expiry — so the printed `locked` figures will not tie out against this run
# alone. For a transcript anyone can check:
#
#   ./network/down.sh && ./network/up.sh \
#     && ./network/deploy-cc.sh settlement && ./network/demo.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"

configure_platform
require_tools
ensure_upstream
export_fabric_env

cd "${TEST_NETWORK_DIR}"

# Upstream's helper scripts are not written for `set -u` — envVar.sh reads
# OVERRIDE_ORG unconditionally — so unset-variable checking is dropped for the
# rest of this script. Error propagation (`set -e -o pipefail`) stays on.
set +u
# shellcheck disable=SC1091
source ./scripts/envVar.sh

ORDERER_CA="${TEST_NETWORK_DIR}/organizations/ordererOrganizations/example.com/tlsca/tlsca.example.com-cert.pem"
PEER0_ORG1_CA="${TEST_NETWORK_DIR}/organizations/peerOrganizations/org1.example.com/tlsca/tlsca.org1.example.com-cert.pem"
PEER0_ORG2_CA="${TEST_NETWORK_DIR}/organizations/peerOrganizations/org2.example.com/tlsca/tlsca.org2.example.com-cert.pem"

BOTH_PEERS=(
  --peerAddresses localhost:7051 --tlsRootCertFiles "${PEER0_ORG1_CA}"
  --peerAddresses localhost:9051 --tlsRootCertFiles "${PEER0_ORG2_CA}"
)
ONE_PEER=(
  --peerAddresses localhost:7051 --tlsRootCertFiles "${PEER0_ORG1_CA}"
)

step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }

# Seconds to wait after an invoke before reading state back. Endorsement returns
# as soon as the lite peers answer, but the write is only visible once the
# committing peer has validated and committed the block. Emulated containers make
# that slow, which is why this is generous.
COMMIT_WAIT="${COMMIT_WAIT:-8}"

# DRUNIX splits the peer in two, and upstream's setGlobals indexes them: peer 0
# is the Lite Peer, which endorses and is where chaincode is installed; peer 1 is
# the Committing Peer, which validates and commits. setGlobals defaults to 1, so
# a bare `setGlobals <org>` points the CLI at a peer with no chaincode on it and
# every query fails with "chaincode definition exists, but chaincode is not
# installed". Always ask for peer 0.
LITE_PEER=0

# invoke <org> <json-args> -- endorsed by both orgs.
#
# Note what this does NOT prove: a successful invoke means the endorsements were
# collected and the transaction was ordered, not that it was committed as valid.
# `--waitForEvent` would tell us, but it times out reliably under emulation, so
# every claim below is instead checked by reading committed state back.
invoke() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}"
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" \
    -c "$1"
  sleep "${COMMIT_WAIT}"
}

query() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}"
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1"
}

# Same query, but with upstream's "Using organization N" chatter kept off stdout
# so the result can be parsed.
query_raw() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

# ---------------------------------------------------------------------------
step "Seeding balances (Phase 0 scaffolding; the token contract replaces this)"
# ---------------------------------------------------------------------------
# SenderOrg funds itself in AED minor units (fils); PayoutBankOrg in INR paise.
invoke 1 '{"Args":["SeedBalance","AED","100000000"]}'
invoke 2 '{"Args":["SeedBalance","INR","1000000000"]}'

echo "SenderOrg AED:"     && query 1 '{"Args":["GetBalance","Org1MSP","AED"]}'
echo "PayoutBankOrg INR:" && query 2 '{"Args":["GetBalance","Org2MSP","INR"]}'

# ---------------------------------------------------------------------------
step "HAPPY PATH — Ravi sends 2,200 AED; 30,000 INR to the college"
# ---------------------------------------------------------------------------
HAPPY="TP-HAPPY-${STAMP}"
PAYEE="sha256:collegefeespayeeref0000000000000000"
EXPIRY=$(( $(date +%s) + 1800 ))

# 2,200 AED = 220000 fils. 30,000 INR = 3000000 paise.
invoke 1 "{\"Args\":[\"CreateTransfer\",\"${HAPPY}\",\"220000\",\"AED\",\"3000000\",\"INR\",\"quote-${STAMP}\",\"${EXPIRY}\",\"Org2MSP\",\"${PAYEE}\"]}"
echo "after CreateTransfer:" && query 1 "{\"Args\":[\"GetTransfer\",\"${HAPPY}\"]}"

invoke 1 "{\"Args\":[\"Prepare\",\"${HAPPY}\"]}"
echo "after Prepare:" && query 1 "{\"Args\":[\"GetTransfer\",\"${HAPPY}\"]}"
echo "SenderOrg AED (expect locked 220000):" && query 1 '{"Args":["GetBalance","Org1MSP","AED"]}'
echo "PayoutBankOrg INR (expect locked 3000000):" && query 2 '{"Args":["GetBalance","Org2MSP","INR"]}'

# ---------------------------------------------------------------------------
step "T2.2 — a single organisation must not be able to commit"
# ---------------------------------------------------------------------------
# The chaincode policy is AND(Org1MSP.peer, Org2MSP.peer). Endorsing with one
# peer only must be rejected by the Validation Service, not by the chaincode.
# The lite peer will endorse this happily — one endorsement is a valid
# endorsement. What must happen is that the Validation Service rejects the
# transaction at commit, so the ledger never changes. That is what we check.
setGlobals 1 "${LITE_PEER}"
peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
  --tls --cafile "${ORDERER_CA}" \
  -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
  "${ONE_PEER[@]}" \
  -c "{\"Args\":[\"Commit\",\"${HAPPY}\"]}" || true
sleep "${COMMIT_WAIT}"

STATE_AFTER_SINGLE_ORG="$(query_raw 1 "{\"Args\":[\"GetTransfer\",\"${HAPPY}\"]}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')"
if [ "${STATE_AFTER_SINGLE_ORG}" = "PREPARED" ]; then
  printf '\033[1;32mOK: single-org commit did not take effect — state is still PREPARED\033[0m\n'
else
  die "single-org commit CHANGED state to ${STATE_AFTER_SINGLE_ORG} — the endorsement policy is not doing its job"
fi

# ---------------------------------------------------------------------------
step "Commit with both organisations endorsing"
# ---------------------------------------------------------------------------
invoke 2 "{\"Args\":[\"Commit\",\"${HAPPY}\"]}"
echo "after Commit:" && query 1 "{\"Args\":[\"GetTransfer\",\"${HAPPY}\"]}"
echo "SenderOrg AED:"        && query 1 '{"Args":["GetBalance","Org1MSP","AED"]}'
echo "PayoutBankOrg AED:"    && query 2 '{"Args":["GetBalance","Org2MSP","AED"]}'
echo "PayoutBankOrg INR:"    && query 2 '{"Args":["GetBalance","Org2MSP","INR"]}'
echo "Beneficiary INR:"      && query 2 "{\"Args\":[\"GetBalance\",\"${PAYEE}\",\"INR\"]}"

# ---------------------------------------------------------------------------
step "ABORT PATH — a prepared transfer that expires is refunded in full"
# ---------------------------------------------------------------------------
ABORT="TP-ABORT-${STAMP}"
SHORT_EXPIRY=$(( $(date +%s) + 120 ))

invoke 1 "{\"Args\":[\"CreateTransfer\",\"${ABORT}\",\"110000\",\"AED\",\"1500000\",\"INR\",\"quote-abort-${STAMP}\",\"${SHORT_EXPIRY}\",\"Org2MSP\",\"${PAYEE}\"]}"
invoke 1 "{\"Args\":[\"Prepare\",\"${ABORT}\"]}"
echo "locked while PREPARED:" && query 1 '{"Args":["GetBalance","Org1MSP","AED"]}'

log "waiting for the transfer to expire at ${SHORT_EXPIRY}"
while [ "$(date +%s)" -le "${SHORT_EXPIRY}" ]; do sleep 5; done

invoke 2 "{\"Args\":[\"Abort\",\"${ABORT}\"]}"
echo "after Abort:" && query 1 "{\"Args\":[\"GetTransfer\",\"${ABORT}\"]}"
echo "SenderOrg AED (locks released, total unchanged):" && query 1 '{"Args":["GetBalance","Org1MSP","AED"]}'
echo "PayoutBankOrg INR (locks released, total unchanged):" && query 2 '{"Args":["GetBalance","Org2MSP","INR"]}'
echo "Beneficiary INR (must be unchanged by the abort):" && query 2 "{\"Args\":[\"GetBalance\",\"${PAYEE}\",\"INR\"]}"

printf '\n\033[1;32m── demo complete\033[0m\n'
