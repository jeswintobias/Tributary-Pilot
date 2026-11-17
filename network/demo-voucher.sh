#!/usr/bin/env bash
# Purpose-bound vouchers against the live network, told as the story the whole
# project is built around.
#
# Ravi, in Dubai, sends money home. Part of it is for his sister's college
# fees; the rest is for his mother. Today a remittance settles to one
# beneficiary and what happens next is a promise made off-chain — which is
# exactly the kind of promise settling on a shared ledger was meant to replace.
#
# A voucher binds the money to its purpose. Value leaves the beneficiary's
# balance at issue and can only reach the recipients the voucher names, before
# the date it names. What is not claimed comes back.
#
#   ./network/setup-fx-compliance.sh && ./network/demo-voucher.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"

PAYEE="sha256:ravi-beneficiary-${STAMP}"
COLLEGE="sha256:college-fees-${STAMP}"
MOTHER="sha256:mother-upi-${STAMP}"

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
V_EXIT=0
fail() { echo "FAIL: $*"; V_EXIT=1; }
ok()   { echo "OK: $*"; }

as_platform() {
  export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
  export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
  export CORE_PEER_ADDRESS="localhost:7051"
  export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
  export CORE_PEER_TLS_ENABLED=true
}

# NOTE ON QUOTING: build the invoke payload into a variable first, then pass
# the variable. Writing "$(try 1 "{\"Args\":[...]}")" looks equivalent and is
# not: the escaped quotes inside a command substitution that is itself inside
# double quotes get mangled, and the JSON arrives at peer truncated at the
# first comma. That produced a refusal whose message never reached the
# assertion, so a passing control looked like a failing one.
try() {
  local org="$1"; shift
  if [ "${org}" = "platform" ]; then as_platform; else setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1; fi
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" -c "$1" 2>&1 | tail -1 || true
}
invoke() { try "$1" "$2" >/dev/null; sleep "${COMMIT_WAIT}"; }

query() {
  setGlobals 1 "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

bal() {
  query "{\"Args\":[\"GetBalance\",\"$1\",\"INR\"]}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])' 2>/dev/null || echo "?"
}

attest() {
  local subject="$1"
  local expires=$(( $(date +%s) + 300 * 24 * 60 * 60 ))
  for kind in KYC SANCTIONS; do
    local args
    args="$(node -e '
const [, subject, kind, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", subject, kind, expires, "case-file-demo"],
}));
' -- "${subject}" "${kind}" "${expires}")"
    invoke platform "${args}"
  done
}

# ---------------------------------------------------------------------------
step "Funding, and settling Ravi's remittance"
# ---------------------------------------------------------------------------
invoke 1 '{"Args":["SeedBalance","AED","100000000"]}'
invoke 2 '{"Args":["SeedBalance","INR","2000000000"]}'
attest "${PAYEE}"

TID="TP-RAVI-${STAMP}"
EXPIRY=$(( $(date +%s) + 600 ))
args="$(create_transfer_args --id "${TID}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
invoke 1 "${args}"
invoke 2 "{\"Args\":[\"Accept\",\"${TID}\"]}"
invoke 1 "{\"Args\":[\"Prepare\",\"${TID}\"]}"
invoke 2 "{\"Args\":[\"Commit\",\"${TID}\"]}"

PAYOUT="$(query "{\"Args\":[\"GetTransfer\",\"${TID}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["payoutLegAmount"])')"
state="$(query "{\"Args\":[\"GetTransfer\",\"${TID}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
[ "${state}" = "SETTLED" ] && ok "${TID} settled: ${PAYOUT} INR arrived for the beneficiary" \
  || fail "${TID} is ${state}"

ARRIVED="$(bal "${PAYEE}")"
echo "beneficiary balance: ${ARRIVED} INR"

# ---------------------------------------------------------------------------
step "Screening the two recipients Ravi named"
# ---------------------------------------------------------------------------
# A voucher must not be a way to route value to somebody who could not have
# received it directly, so each recipient is screened like any beneficiary.
attest "${COLLEGE}"
attest "${MOTHER}"
ok "the college and the mother are both attested"

# ---------------------------------------------------------------------------
step "Binding the money to its purpose"
# ---------------------------------------------------------------------------
TUITION=$(( PAYOUT / 2 ))
FAMILY=$(( PAYOUT - TUITION ))
VID="V-${STAMP}"
# Short, so the refund path can be shown without a long wait. A real voucher
# would last as long as the term it pays for.
VOUCHER_EXPIRY=$(( $(date +%s) + 90 ))

echo "${TUITION} INR bound to TUITION for the college"
echo "${FAMILY} INR bound to FAMILY for his mother"
issue_args="$(node -e '
const [, vid, tid, college, mother, tuition, family, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueVoucher", vid, tid, JSON.stringify([
    { purpose: "TUITION", payeeRef: college, amount: Number(tuition) },
    { purpose: "FAMILY", payeeRef: mother, amount: Number(family) },
  ]), expires],
}));
' -- "${VID}" "${TID}" "${COLLEGE}" "${MOTHER}" "${TUITION}" "${FAMILY}" "${VOUCHER_EXPIRY}")"
out="$(try 2 "${issue_args}")"
if echo "${out}" | grep -q "status:200"; then
  ok "voucher ${VID} issued"
else
  fail "IssueVoucher: ${out}"
  echo "V_EXIT=${V_EXIT}"; exit 1
fi
sleep "${COMMIT_WAIT}"

AFTER_BIND="$(bal "${PAYEE}")"
BOUND="$(bal "voucher:${VID}")"
echo "beneficiary ${ARRIVED} -> ${AFTER_BIND}, voucher holds ${BOUND}"
[ "${AFTER_BIND}" = "0" ] && ok "the money is no longer the beneficiary's to spend freely" \
  || fail "beneficiary still holds ${AFTER_BIND}"
[ "${BOUND}" = "${PAYOUT}" ] && ok "the whole payout is bound to its purposes" \
  || fail "voucher holds ${BOUND}, expected ${PAYOUT}"

# ---------------------------------------------------------------------------
step "A voucher that does not account for the whole payout"
# ---------------------------------------------------------------------------
short_args="$(node -e '
const [, tid, college, amount, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueVoucher", "V-SHORT", tid, JSON.stringify([
    { purpose: "TUITION", payeeRef: college, amount: Number(amount) },
  ]), expires],
}));
' -- "${TID}" "${COLLEGE}" "1" "${VOUCHER_EXPIRY}")"
out="$(try 2 "${short_args}")"
if echo "${out}" | grep -q "must account for all of it"; then
  ok "a voucher that leaves part of the payout unaccounted for is refused"
else
  fail "a short voucher was not refused: ${out}"
fi

# ---------------------------------------------------------------------------
step "The college claims its fees"
# ---------------------------------------------------------------------------
invoke 2 "{\"Args\":[\"RedeemVoucher\",\"${VID}\",\"0\"]}"
COLLEGE_BAL="$(bal "${COLLEGE}")"
MOTHER_BAL="$(bal "${MOTHER}")"
echo "college ${COLLEGE_BAL}, mother ${MOTHER_BAL}"
[ "${COLLEGE_BAL}" = "${TUITION}" ] && ok "the college received exactly its share" \
  || fail "college has ${COLLEGE_BAL}, expected ${TUITION}"
[ "${MOTHER_BAL}" = "0" ] && ok "the family's share has not moved" \
  || fail "the family's share moved early: ${MOTHER_BAL}"

redeem0_args="{\"Args\":[\"RedeemVoucher\",\"${VID}\",\"0\"]}"
out="$(try 2 "${redeem0_args}")"
if echo "${out}" | grep -q "already redeemed"; then
  ok "the same split cannot be claimed twice"
else
  fail "a repeat redemption was not refused: ${out}"
fi

# ---------------------------------------------------------------------------
step "A sanctions hit on a recipient who has not claimed yet"
# ---------------------------------------------------------------------------
# A voucher can outlive a screening decision, and the money moves at
# redemption rather than at issue.
invoke platform "{\"Args\":[\"RevokeAttestation\",\"${MOTHER}\",\"SANCTIONS\",\"ofac-match\"]}"
redeem1_args="{\"Args\":[\"RedeemVoucher\",\"${VID}\",\"1\"]}"
out="$(try 2 "${redeem1_args}")"
if echo "${out}" | grep -q "redemption refused"; then
  ok "redemption by a newly sanctioned recipient is refused"
else
  fail "redemption after revocation was not refused: ${out}"
fi
[ "$(bal "${MOTHER}")" = "0" ] && ok "she received nothing" \
  || fail "the sanctioned recipient was paid"

# ---------------------------------------------------------------------------
step "What was never claimed comes back"
# ---------------------------------------------------------------------------
refund_args="{\"Args\":[\"RefundVoucher\",\"${VID}\"]}"
out="$(try 2 "${refund_args}")"
if echo "${out}" | grep -q "does not expire until"; then
  ok "refunding early is refused: the recipient may simply not have claimed yet"
else
  fail "an early refund was not refused: ${out}"
fi

echo "waiting for the voucher to expire at ${VOUCHER_EXPIRY}..."
while [ "$(date +%s)" -lt "${VOUCHER_EXPIRY}" ]; do sleep 5; done
invoke 2 "{\"Args\":[\"RefundVoucher\",\"${VID}\"]}"

FINAL_PAYEE="$(bal "${PAYEE}")"
FINAL_VOUCHER="$(bal "voucher:${VID}")"
echo "beneficiary ${FINAL_PAYEE}, voucher ${FINAL_VOUCHER}, college ${COLLEGE_BAL}"
[ "${FINAL_PAYEE}" = "${FAMILY}" ] && ok "the unclaimed share returned to the beneficiary" \
  || fail "beneficiary has ${FINAL_PAYEE}, expected ${FAMILY}"
[ "${FINAL_VOUCHER}" = "0" ] && ok "the voucher account is empty" \
  || fail "voucher still holds ${FINAL_VOUCHER}"

query "{\"Args\":[\"GetVoucher\",\"${VID}\"]}" | python3 -m json.tool

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
echo "V_EXIT=${V_EXIT}"
exit "${V_EXIT}"
