#!/usr/bin/env bash
# One-time setup for Phase D: register the FX oracle's key and attest the
# beneficiaries the demos pay.
#
# Both are acts of PlatformOrg (Org3MSP), which is the only organisation the
# chaincode lets register an oracle or issue an attestation. It runs no peer of
# its own and does not need one: it signs with its own identity and another
# organisation's peer endorses.
#
# After this, a transfer can be created (it has a signed rate to quote) and
# prepared (its beneficiary has been identified and screened). Before it,
# neither works, which is the point.
#
#   ./network/deploy-cc.sh settlement && ./network/add-platform-org.sh \
#     && ./network/setup-fx-compliance.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
ORACLE_DIR="${REPO_DIR}/services/oracle"

ORG3_MSP="Org3MSP"
ORG3_HOME="${TRIBUTARY_HOME}/platformorg"
ORG3_ADMIN_MSP="${ORG3_HOME}/organizations/peerOrganizations/org3.example.com/users/Admin@org3.example.com/msp"

# The beneficiaries the demo scripts pay. Salted hashes computed off-chain; no
# personally identifying information reaches the ledger.
PAYEES=(
  "sha256:collegefeespayeeref0000000000000000"
  "sha256:pipeline-closed-account"
)

configure_platform
require_tools
ensure_upstream
export_fabric_env

cd "${TEST_NETWORK_DIR}"
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

COMMIT_WAIT="${COMMIT_WAIT:-8}"
step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
S_EXIT=0
fail() { echo "FAIL: $*"; S_EXIT=1; }
ok()   { echo "OK: $*"; }

[ -d "${ORG3_ADMIN_MSP}" ] || {
  echo "PlatformOrg's admin MSP is missing at ${ORG3_ADMIN_MSP}"
  echo "Run ./network/add-platform-org.sh first."
  exit 1
}

# PlatformOrg signs with its own identity and borrows Org1's Lite Peer as
# transport.
as_platform() {
  export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
  export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
  export CORE_PEER_ADDRESS="localhost:7051"
  export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
  export CORE_PEER_TLS_ENABLED=true
}

invoke_platform() {
  as_platform
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" -c "$1" 2>&1 | tail -1
  sleep "${COMMIT_WAIT}"
}

query_as() {
  local who="$1"; shift
  if [ "${who}" = "platform" ]; then as_platform; else setGlobals 1 0 >/dev/null 2>&1; fi
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

# ---------------------------------------------------------------------------
step "Reading the oracle's public key"
# ---------------------------------------------------------------------------
# The key is generated on first use and never committed. This reads it from the
# oracle service's own key file rather than asking a running server, so setup
# does not depend on the service being up.
ORACLE_ID="$(cd "${ORACLE_DIR}" && node src/pubkey.js --id 2>/dev/null)"
ORACLE_PEM="$(cd "${ORACLE_DIR}" && node src/pubkey.js 2>/dev/null)"
if [ -z "${ORACLE_ID}" ] || [ -z "${ORACLE_PEM}" ]; then
  fail "could not read the oracle key from ${ORACLE_DIR}"
  echo "S_EXIT=1"; exit 1
fi
echo "oracle ${ORACLE_ID}"
ok "public key read ($(echo "${ORACLE_PEM}" | wc -l | tr -d ' ') lines of PEM)"

# ---------------------------------------------------------------------------
step "Registering the oracle on chain, as PlatformOrg"
# ---------------------------------------------------------------------------
REGISTER_ARGS="$(node -e '
const [, id, pem] = process.argv;
process.stdout.write(JSON.stringify({ Args: ["RegisterOracle", id, pem] }));
' -- "${ORACLE_ID}" "${ORACLE_PEM}")"

out="$(invoke_platform "${REGISTER_ARGS}")"
if echo "${out}" | grep -q "already registered"; then
  ok "oracle ${ORACLE_ID} was already registered"
elif echo "${out}" | grep -q "status:200"; then
  ok "oracle ${ORACLE_ID} registered"
else
  fail "RegisterOracle: ${out}"
fi

registered="$(query_as org1 "{\"Args\":[\"GetOracle\",\"${ORACLE_ID}\"]}")"
if echo "${registered}" | grep -q "${ORACLE_ID}"; then
  ok "GetOracle confirms the key is on the ledger"
else
  fail "GetOracle returned: ${registered}"
fi

# ---------------------------------------------------------------------------
step "Refusing registration by an organisation that is not an oracle MSP"
# ---------------------------------------------------------------------------
# Worth proving rather than asserting: the whole mechanism rests on who may
# install a trusted key.
setGlobals 1 0 >/dev/null 2>&1
ROGUE_ARGS="$(node -e '
const [, pem] = process.argv;
process.stdout.write(JSON.stringify({ Args: ["RegisterOracle", "oracle-rogue", pem] }));
' -- "${ORACLE_PEM}")"
rogue="$(peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
  --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
  "${BOTH_PEERS[@]}" -c "${ROGUE_ARGS}" 2>&1 | tail -1 || true)"
if echo "${rogue}" | grep -q "may not register an oracle key"; then
  ok "SenderOrg was refused: $(echo "${rogue}" | grep -o 'may not register an oracle key')"
else
  fail "a non-oracle org was not refused: ${rogue}"
fi

# ---------------------------------------------------------------------------
step "Attesting the demo beneficiaries, as PlatformOrg"
# ---------------------------------------------------------------------------
# A year from now, which is inside the contract's cap. Both kinds are required
# before a payout leg may move.
EXPIRY=$(( $(date +%s) + 300 * 24 * 60 * 60 ))
for payee in "${PAYEES[@]}"; do
  for kind in KYC SANCTIONS; do
    args="$(node -e '
const [, subject, kind, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", subject, kind, expires, "case-file-demo"],
}));
' -- "${payee}" "${kind}" "${EXPIRY}")"
    out="$(invoke_platform "${args}")"
    if echo "${out}" | grep -qE "status:200|is live until"; then
      ok "${kind} attested for ${payee}"
    else
      fail "IssueAttestation ${kind} ${payee}: ${out}"
    fi
  done
done

# ---------------------------------------------------------------------------
step "Checking each beneficiary reads as payable"
# ---------------------------------------------------------------------------
for payee in "${PAYEES[@]}"; do
  status="$(query_as org1 "{\"Args\":[\"CheckBeneficiary\",\"${payee}\"]}")"
  clear="$(echo "${status}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["clear"])' 2>/dev/null || echo "?")"
  if [ "${clear}" = "True" ]; then
    ok "${payee} is clear to be paid"
  else
    fail "${payee} reads as not clear: ${status}"
  fi
done

# ---------------------------------------------------------------------------
step "Refusing an attestation from an organisation that is not an attestor"
# ---------------------------------------------------------------------------
setGlobals 1 0 >/dev/null 2>&1
self_args="$(node -e '
const [, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", "sha256:self-certified", "KYC", expires, "trust-me"],
}));
' -- "${EXPIRY}")"
selfcert="$(peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
  --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
  "${BOTH_PEERS[@]}" -c "${self_args}" 2>&1 | tail -1 || true)"
if echo "${selfcert}" | grep -q "not an approved attestor"; then
  ok "SenderOrg cannot attest its own beneficiaries"
else
  fail "a non-attestor was not refused: ${selfcert}"
fi

echo
echo "S_EXIT=${S_EXIT}"
exit "${S_EXIT}"
