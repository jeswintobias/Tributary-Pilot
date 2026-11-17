#!/usr/bin/env bash
# Phase D against the live network: a transfer can only exist at a rate
# somebody signed, and can only settle to a beneficiary somebody screened.
#
# Everything here is a REFUSAL except where marked. That is the point: the
# earlier demos show the system working, and this one shows what it declines to
# do. A control nobody has watched fail is not a control.
#
#   ./network/setup-fx-compliance.sh && ./network/demo-compliance.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"
PAYEE="sha256:compliance-payee-${STAMP}"
FRESH_PAYEE="sha256:unscreened-${STAMP}"

ORG3_MSP="Org3MSP"
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

ORDERER_CA="${TEST_NETWORK_DIR}/organizations/ordererOrganizations/example.com/tlsca/tlsca.example.com-cert.pem"
PEER0_ORG1_CA="${TEST_NETWORK_DIR}/organizations/peerOrganizations/org1.example.com/tlsca/tlsca.org1.example.com-cert.pem"
PEER0_ORG2_CA="${TEST_NETWORK_DIR}/organizations/peerOrganizations/org2.example.com/tlsca/tlsca.org2.example.com-cert.pem"
BOTH_PEERS=(
  --peerAddresses localhost:7051 --tlsRootCertFiles "${PEER0_ORG1_CA}"
  --peerAddresses localhost:9051 --tlsRootCertFiles "${PEER0_ORG2_CA}"
)

LITE_PEER=0
COMMIT_WAIT="${COMMIT_WAIT:-8}"
step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
D_EXIT=0
fail() { echo "FAIL: $*"; D_EXIT=1; }
ok()   { echo "OK: $*"; }

as_platform() {
  export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
  export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
  export CORE_PEER_ADDRESS="localhost:7051"
  export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
  export CORE_PEER_TLS_ENABLED=true
}

# try runs an invoke and returns its output WITHOUT aborting on failure, so a
# refusal can be inspected rather than killing the script.
try() {
  local org="$1"; shift
  if [ "${org}" = "platform" ]; then as_platform; else setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1; fi
  local out
  out="$(peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" -c "$1" 2>&1 | tail -1 || true)"
  echo "${out}"
}

invoke() { try "$1" "$2" >/dev/null; sleep "${COMMIT_WAIT}"; }

# attest issues the two required attestations for a beneficiary, as PlatformOrg.
#
# Each demo attests its OWN per-run references rather than relying on a shared
# set, because a revocation is permanent for a subject: the contract refuses to
# re-issue over one, so that a sanctions hit cannot be papered over by writing
# a fresh attestation on top of it. That is the behaviour we want and it makes
# any shared reference single-use, so the references are stamped per run.
attest() {
  local subject="$1"
  local expires=$(( $(date +%s) + 300 * 24 * 60 * 60 ))
  local kind args
  for kind in KYC SANCTIONS; do
    args="$(node -e '
const [, subject, kind, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", subject, kind, expires, "case-file-demo"],
}));
' -- "${subject}" "${kind}" "${expires}")"
    invoke platform "${args}"
  done
}


query() {
  setGlobals 1 "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

# refuses asserts that an invoke failed for the stated reason.
# NOTE ON QUOTING: build the invoke payload into a variable first, then pass
# the variable. Writing "$(try 1 "{\"Args\":[...]}")" looks equivalent and is
# not: the escaped quotes inside a command substitution that is itself inside
# double quotes get mangled, and the JSON arrives at peer truncated at the
# first comma. That produced a refusal whose message never reached the
# assertion, so a passing control looked like a failing one.
refuses() {
  local label="$1" expect="$2" out="$3"
  if echo "${out}" | grep -q "${expect}"; then
    ok "${label}"
  else
    fail "${label} — expected ${expect}, got: ${out}"
  fi
}

EXPIRY=$(( $(date +%s) + 600 ))

# ---------------------------------------------------------------------------
step "Funding"
# ---------------------------------------------------------------------------
invoke 1 '{"Args":["SeedBalance","AED","100000000"]}'
invoke 2 '{"Args":["SeedBalance","INR","2000000000"]}'
ok "balances seeded"
attest "${PAYEE}"
ok "this run's beneficiary ${PAYEE} is screened"

# ---------------------------------------------------------------------------
step "A rate nobody signed"
# ---------------------------------------------------------------------------
# The quote is perfect in every respect except that the key behind it is not
# the registered oracle's. Anyone can produce a well-formed signature; only the
# oracle can produce one that verifies.
args="$(create_transfer_args --id "TP-FORGE-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" --forge)"
refuses "a forged quote is refused" "is not signed by oracle" "$(try 1 "${args}")"

# ---------------------------------------------------------------------------
step "A rate that was true yesterday"
# ---------------------------------------------------------------------------
STALE=$(( $(date +%s) - 60 ))
args="$(create_transfer_args --id "TP-STALE-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --quote-expires "${STALE}")"
refuses "an expired quote is refused" "only true for a moment" "$(try 1 "${args}")"

# ---------------------------------------------------------------------------
step "A genuine rate, attached to legs that do not match it"
# ---------------------------------------------------------------------------
# This is what stops a signed quote being decoration. The signature verifies,
# the quote is current, and the transfer simply is not at that rate.
args="$(create_transfer_args --id "TP-MISMATCH-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --payout 3000000)"
refuses "legs that contradict the quoted rate are refused" "exceeds the" "$(try 1 "${args}")"

# ---------------------------------------------------------------------------
step "Within the tolerance the oracle stated"
# ---------------------------------------------------------------------------
# 25 bps of allowance, and a payout 10 bps away. Accepted — the only thing in
# this script that is.
WITHIN=5000979   # 5003482 minus ~5 bps
args="$(create_transfer_args --id "TP-SLIP-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --payout "${WITHIN}" --slippage 25)"
out="$(try 1 "${args}")"
if echo "${out}" | grep -q "status:200"; then
  ok "a payout inside the stated tolerance is accepted"
else
  fail "slippage within bounds was refused: ${out}"
fi
sleep "${COMMIT_WAIT}"

# ---------------------------------------------------------------------------
step "A beneficiary nobody screened"
# ---------------------------------------------------------------------------
UNSCREENED="TP-UNSCREENED-${STAMP}"
args="$(create_transfer_args --id "${UNSCREENED}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${FRESH_PAYEE}")"
out="$(try 1 "${args}")"
if echo "${out}" | grep -q "status:200"; then
  ok "the transfer was captured: compliance is checked at prepare, not capture"
else
  fail "CreateTransfer: ${out}"
fi
sleep "${COMMIT_WAIT}"
invoke 2 "{\"Args\":[\"Accept\",\"${UNSCREENED}\"]}"
prep_args="{\"Args\":[\"Prepare\",\"${UNSCREENED}\"]}"
refuses "preparing for an unscreened beneficiary is refused" "has no KYC attestation" \
  "$(try 1 "${prep_args}")"

echo "CheckBeneficiary says why:"
query "{\"Args\":[\"CheckBeneficiary\",\"${FRESH_PAYEE}\"]}"

# Nothing was locked, which is the reason the check sits at prepare.
locked="$(query "{\"Args\":[\"GetBalance\",\"escrow:${UNSCREENED}\",\"AED\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])' 2>/dev/null || echo "?")"
[ "${locked}" = "0" ] && ok "no funds were locked for an unpayable beneficiary" \
  || fail "escrow holds ${locked} after a refused prepare"

# ---------------------------------------------------------------------------
step "A sanctions hit landing mid-settlement"
# ---------------------------------------------------------------------------
# The beneficiary is screened, the transfer is prepared and the counterparty's
# funds are locked. Then the screening provider finds something. Checking
# compliance only at prepare would pay this one.
MID="TP-MIDHIT-${STAMP}"
args="$(create_transfer_args --id "${MID}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
invoke 1 "${args}"
invoke 2 "{\"Args\":[\"Accept\",\"${MID}\"]}"
invoke 1 "{\"Args\":[\"Prepare\",\"${MID}\"]}"

state="$(query "{\"Args\":[\"GetTransfer\",\"${MID}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
[ "${state}" = "PREPARED" ] && ok "${MID} is PREPARED, funds locked" \
  || fail "${MID} is ${state}, expected PREPARED"

PAID_BEFORE="$(query "{\"Args\":[\"GetBalance\",\"${PAYEE}\",\"INR\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])' 2>/dev/null || echo "0")"
echo "beneficiary holds ${PAID_BEFORE} INR before the attempt"

echo "the screening provider revokes the sanctions attestation..."
invoke platform "{\"Args\":[\"RevokeAttestation\",\"${PAYEE}\",\"SANCTIONS\",\"ofac-match\"]}"

commit_args="{\"Args\":[\"Commit\",\"${MID}\"]}"
refuses "committing after a revocation is refused" "commit refused" \
  "$(try 2 "${commit_args}")"

# Compared against what the beneficiary held BEFORE the commit was attempted,
# not against zero: this ledger carries balances from earlier demo runs, and
# asserting zero would be asserting a fresh network rather than asserting the
# control.
paid_after="$(query "{\"Args\":[\"GetBalance\",\"${PAYEE}\",\"INR\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])' 2>/dev/null || echo "?")"
[ "${paid_after}" = "${PAID_BEFORE}" ] \
  && ok "the beneficiary received nothing (still ${paid_after} INR)" \
  || fail "the beneficiary moved from ${PAID_BEFORE} to ${paid_after} INR despite the revocation"

# ---------------------------------------------------------------------------
step "The refund still works"
# ---------------------------------------------------------------------------
# A revocation must not strand the counterparty's money. Value returning to the
# person it came from is not a payout and is not gated on compliance.
echo "waiting for the transfer to expire so it can be aborted..."
while [ "$(date +%s)" -lt "${EXPIRY}" ]; do sleep 10; done
abort_args="{\"Args\":[\"Abort\",\"${MID}\"]}"
out="$(try 1 "${abort_args}")"
if echo "${out}" | grep -q "status:200"; then
  ok "the locked funds were refunded despite the revocation"
else
  fail "Abort after revocation: ${out}"
fi
sleep "${COMMIT_WAIT}"

escrow="$(query "{\"Args\":[\"GetBalance\",\"escrow:${MID}\",\"AED\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])' 2>/dev/null || echo "?")"
[ "${escrow}" = "0" ] && ok "escrow is empty: the refund completed" \
  || fail "escrow still holds ${escrow}"

# ---------------------------------------------------------------------------
step "Conservation across every account"
# ---------------------------------------------------------------------------
query '{"Args":["ListBalances"]}' | python3 -c '
import sys, json, collections
accounts = json.load(sys.stdin) or []
per = collections.defaultdict(int)
for a in accounts:
    per[a["ccy"]] += a["balance"]
bad = False
for ccy in sorted(per):
    verdict = "BALANCED" if per[ccy] == 0 else "NOT BALANCED"
    if per[ccy] != 0:
        bad = True
    print("  %s sums to %d   %s" % (ccy, per[ccy], verdict))
sys.exit(1 if bad else 0)
' || fail "the books do not balance"

echo
echo "D_EXIT=${D_EXIT}"
exit "${D_EXIT}"
