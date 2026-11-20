#!/usr/bin/env bash
# The adversarial suite: everything this ledger must refuse, in one run.
#
# The other scripts demonstrate the system working. This one attacks it. Every
# case below is an attempt to take money, move money that is not yours, settle
# something nobody agreed to, or widen a limit single-handedly — and every one
# must be refused.
#
# THE DISCIPLINE THAT MAKES THIS WORTH RUNNING
#
# A failed attack is not a passed test. The attack has to fail FOR THE RIGHT
# REASON, in the layer that is supposed to stop it. A malformed argument, a
# channel-level rejection, a peer that happened to be down — each of those
# "fails" the attack while proving nothing about the control. So every case
# names the message it expects, and a refusal that does not match it is
# reported as a FAILURE of the suite, not a success.
#
# Cases are tagged with the threat they cover, so a reader can go from
# docs/THREAT_MODEL.md to the thing that actually tries it.
#
#   ./network/setup-fx-compliance.sh && ./network/adversarial.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"

PAYEE="sha256:adversarial-payee-${STAMP}"
PAYEE2="sha256:adversarial-second-${STAMP}"
UNSCREENED="sha256:adversarial-unscreened-${STAMP}"

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
ONE_PEER=(
  --peerAddresses localhost:7051 --tlsRootCertFiles "${PEER0_ORG1_CA}"
)

LITE_PEER=0
COMMIT_WAIT="${COMMIT_WAIT:-8}"

step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
ATTACKS=0
REFUSED=0
A_EXIT=0
fail() { echo "  FAIL: $*"; A_EXIT=1; }

# NOTE ON QUOTING: build every invoke payload into a variable first. Inline
# escaped JSON inside a command substitution that is itself inside double
# quotes gets mangled and arrives at peer truncated at the first comma, which
# produces a refusal whose message never reaches the assertion.

as_org() {
  case "$1" in
    platform)
      export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
      export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
      export CORE_PEER_ADDRESS="localhost:7051"
      export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
      export CORE_PEER_TLS_ENABLED=true
      ;;
    *) setGlobals "$1" "${LITE_PEER}" >/dev/null 2>&1 ;;
  esac
}

# run_invoke <org> <payload> [one-peer] -> prints the last line of output
run_invoke() {
  local org="$1" payload="$2" peers="${3:-both}"
  as_org "${org}"
  local peerargs=("${BOTH_PEERS[@]}")
  [ "${peers}" = "one" ] && peerargs=("${ONE_PEER[@]}")
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${peerargs[@]}" -c "${payload}" 2>&1 | tail -1 || true
}

ok_invoke() { run_invoke "$1" "$2" >/dev/null; sleep "${COMMIT_WAIT}"; }

query() {
  as_org 1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

bal() {
  query "{\"Args\":[\"GetBalance\",\"$1\",\"$2\"]}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])' 2>/dev/null || echo "0"
}

# attack <threat-id> <description> <expected-message> <org> <payload> [one-peer]
#
# Asserts the call was refused AND that the refusal says what it should. A
# refusal from the wrong layer is a failure of this suite.
attack() {
  local threat="$1" what="$2" expect="$3" org="$4" payload="$5" peers="${6:-both}"
  ATTACKS=$(( ATTACKS + 1 ))
  local out
  out="$(run_invoke "${org}" "${payload}" "${peers}")"

  if echo "${out}" | grep -q "status:200"; then
    fail "[${threat}] ${what} — SUCCEEDED. It should have been refused."
    return
  fi
  if ! echo "${out}" | grep -q "${expect}"; then
    fail "[${threat}] ${what} — refused, but not for the stated reason."
    echo "        expected: ${expect}"
    echo "        got:      $(echo "${out}" | tail -c 220)"
    return
  fi
  REFUSED=$(( REFUSED + 1 ))
  printf '  \033[0;32mrefused\033[0m  [%-7s] %s\n' "${threat}" "${what}"
}

# attack_query is the same, for calls that are queries rather than invokes.
attack_query() {
  local threat="$1" what="$2" expect="$3" payload="$4"
  ATTACKS=$(( ATTACKS + 1 ))
  as_org 1
  local out
  out="$(peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "${payload}" 2>&1 | tail -1 || true)"
  if ! echo "${out}" | grep -q "${expect}"; then
    fail "[${threat}] ${what} — expected ${expect}, got: $(echo "${out}" | tail -c 200)"
    return
  fi
  REFUSED=$(( REFUSED + 1 ))
  printf '  \033[0;32mrefused\033[0m  [%-7s] %s\n' "${threat}" "${what}"
}

attest() {
  local subject="$1"
  local expires=$(( $(date +%s) + 300 * 24 * 60 * 60 ))
  local kind args
  for kind in KYC SANCTIONS; do
    args="$(node -e '
const [, subject, kind, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", subject, kind, expires, "adversarial-suite"],
}));
' -- "${subject}" "${kind}" "${expires}")"
    ok_invoke platform "${args}"
  done
}

[ -d "${ORG3_ADMIN_MSP}" ] || die "PlatformOrg is missing; run ./network/add-platform-org.sh"

# ---------------------------------------------------------------------------
step "Setting up: funds, screened beneficiaries, one settled transfer"
# ---------------------------------------------------------------------------
ok_invoke 1 '{"Args":["SeedBalance","AED","100000000"]}'

# How much INR the payout org needs is derived from what the sender org holds,
# not guessed. Prepare locks BOTH legs, so a double-spend attempt only proves
# anything if the payout org can afford two payout legs while the sender can
# afford only one sender leg. At roughly 22.74 INR to the dirham that means the
# payout org needs more than 23x the sender's balance; 30x leaves room.
SENDER_AED="$(bal Org1MSP AED)"
NEEDED_INR=$(( SENDER_AED * 30 ))
seed_args="{\"Args\":[\"SeedBalance\",\"INR\",\"${NEEDED_INR}\"]}"
ok_invoke 2 "${seed_args}"
attest "${PAYEE}"
attest "${PAYEE2}"
echo "funded: ${SENDER_AED} AED at the sender, ${NEEDED_INR} INR seeded to the payout bank"
echo "${PAYEE} is screened"

EXPIRY=$(( $(date +%s) + 700 ))

# A transfer that reaches PREPARED, for the attacks that need locked funds.
LOCKED="TP-ADV-LOCKED-${STAMP}"
args="$(create_transfer_args --id "${LOCKED}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
ok_invoke 1 "${args}"
ok_invoke 2 "{\"Args\":[\"Accept\",\"${LOCKED}\"]}"
ok_invoke 1 "{\"Args\":[\"Prepare\",\"${LOCKED}\"]}"
state="$(query "{\"Args\":[\"GetTransfer\",\"${LOCKED}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
[ "${state}" = "PREPARED" ] || die "setup failed: ${LOCKED} is ${state}, expected PREPARED"
echo "${LOCKED} is PREPARED with funds locked"

# A transfer sitting at CAPTURED, for the attacks that need an unaccepted one.
CAPTURED="TP-ADV-CAPTURED-${STAMP}"
args="$(create_transfer_args --id "${CAPTURED}" --sender 110000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
ok_invoke 1 "${args}"
echo "${CAPTURED} is CAPTURED"

# ---------------------------------------------------------------------------
step "The rate"
# ---------------------------------------------------------------------------
args="$(create_transfer_args --id "TP-ADV-FORGE-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" --forge)"
attack "T10.1.1" "a rate signed by a key the chain does not know" \
  "is not signed by oracle" 1 "${args}"

STALE=$(( $(date +%s) - 60 ))
args="$(create_transfer_args --id "TP-ADV-STALE-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --quote-expires "${STALE}")"
attack "T10.1.2" "yesterday's rate, correctly signed" \
  "only true for a moment" 1 "${args}"

args="$(create_transfer_args --id "TP-ADV-MISMATCH-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --payout 9000000)"
attack "T10.1.3" "a genuine quote attached to legs that contradict it" \
  "exceeds the" 1 "${args}"

# Reuse: deal one quote, then present the same quote for a second transfer.
REUSE_Q="q-adv-reuse-${STAMP}"
args="$(create_transfer_args --id "TP-ADV-REUSE-A-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --quote-id "${REUSE_Q}")"
ok_invoke 1 "${args}"
args="$(create_transfer_args --id "TP-ADV-REUSE-B-${STAMP}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}" \
  --quote-id "${REUSE_Q}")"
attack "T10.1.6" "spending one dealt quote twice" \
  "a quote is single use" 1 "${args}"

ORACLE_PEM="$(cd "${REPO_DIR}/services/oracle" && node src/pubkey.js 2>/dev/null)"
rogue_args="$(node -e '
const [, pem] = process.argv;
process.stdout.write(JSON.stringify({ Args: ["RegisterOracle", "oracle-rogue", pem] }));
' -- "${ORACLE_PEM}")"
attack "T10.1.9" "a party installing its own trusted signing key" \
  "may not register an oracle key" 1 "${rogue_args}"

# ---------------------------------------------------------------------------
step "The beneficiary"
# ---------------------------------------------------------------------------
UNSCR_TID="TP-ADV-UNSCR-${STAMP}"
args="$(create_transfer_args --id "${UNSCR_TID}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${UNSCREENED}")"
ok_invoke 1 "${args}"
ok_invoke 2 "{\"Args\":[\"Accept\",\"${UNSCR_TID}\"]}"
prep_args="{\"Args\":[\"Prepare\",\"${UNSCR_TID}\"]}"
attack "T10.2.1" "locking funds for a beneficiary nobody screened" \
  "has no KYC attestation" 1 "${prep_args}"

selfcert_args="$(node -e '
const [, subject, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", subject, "KYC", expires, "trust-me"],
}));
' -- "${UNSCREENED}" "$(( $(date +%s) + 86400 ))")"
attack "T10.2.4" "a party clearing its own beneficiary" \
  "not an approved attestor" 1 "${selfcert_args}"

revoke_args="{\"Args\":[\"RevokeAttestation\",\"${PAYEE}\",\"KYC\",\"inconvenient\"]}"
attack "T10.2.5" "a party revoking somebody else's attestation" \
  "may not revoke it" 1 "${revoke_args}"

# ---------------------------------------------------------------------------
step "Settlement integrity"
# ---------------------------------------------------------------------------
commit_args="{\"Args\":[\"Commit\",\"${CAPTURED}\"]}"
attack "T1.1" "committing a transfer that was never prepared" \
  "Commit requires PREPARED" 2 "${commit_args}"

prep_args="{\"Args\":[\"Prepare\",\"${CAPTURED}\"]}"
attack "T1.2" "locking the counterparty's funds before it accepts" \
  "Prepare requires ACCEPTED" 1 "${prep_args}"

replay_args="$(create_transfer_args --id "${LOCKED}" --sender 220000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
attack "T1.3" "replaying a transfer ID" \
  "already exists" 1 "${replay_args}"

abort_args="{\"Args\":[\"Abort\",\"${LOCKED}\"]}"
attack "T1.4" "unilaterally aborting a prepared transfer before it expires" \
  "unilateral early abort is refused" 1 "${abort_args}"

# Double-spend: two transfers each for just over half of what the payer has
# available, so the first locks and the second cannot. Sized from the live
# balance rather than hardcoded, because this ledger carries balances from
# earlier runs.
AVAIL="$(bal Org1MSP AED)"
HALF_PLUS=$(( AVAIL / 2 + 1000 ))
DBL_A="TP-ADV-DBL-A-${STAMP}"
DBL_B="TP-ADV-DBL-B-${STAMP}"
echo "  payer has ${AVAIL} AED available; attempting ${HALF_PLUS} twice"
for id in "${DBL_A}" "${DBL_B}"; do
  args="$(create_transfer_args --id "${id}" --sender "${HALF_PLUS}" --sender-ccy AED \
    --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
  ok_invoke 1 "${args}"
  ok_invoke 2 "{\"Args\":[\"Accept\",\"${id}\"]}"
done
ok_invoke 1 "{\"Args\":[\"Prepare\",\"${DBL_A}\"]}"
first_state="$(query "{\"Args\":[\"GetTransfer\",\"${DBL_A}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
if [ "${first_state}" != "PREPARED" ]; then
  fail "[T1.5] the setup for the double-spend did not lock: ${DBL_A} is ${first_state}"
fi
prep_args="{\"Args\":[\"Prepare\",\"${DBL_B}\"]}"
attack "T1.5" "spending the same balance twice" \
  "available" 1 "${prep_args}"

reserved_args="$(create_transfer_args --id "TP-ADV-RESERVED-${STAMP}" --sender 1000 --sender-ccy AED \
  --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "issuance:INR")"
attack "T1.6" "naming the issuance account as the beneficiary" \
  "reserved account prefix" 1 "${reserved_args}"

# ---------------------------------------------------------------------------
step "Authorisation and endorsement"
# ---------------------------------------------------------------------------
nonparty_args="{\"Args\":[\"Commit\",\"${LOCKED}\"]}"
attack "T2.1" "an org that is not a party settling somebody else's transfer" \
  "is not a party" platform "${nonparty_args}"

# Under-endorsed: a commit collected from one organisation only. The chaincode
# cannot see how many endorsed, so this is the Validation Service's job and the
# refusal looks different — the invoke is accepted and the transaction is
# marked invalid, leaving the transfer PREPARED.
ATTACKS=$(( ATTACKS + 1 ))
as_org 2
single_args="{\"Args\":[\"Commit\",\"${LOCKED}\"]}"
peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
  --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
  "${ONE_PEER[@]}" -c "${single_args}" >/dev/null 2>&1 || true
sleep "${COMMIT_WAIT}"
after="$(query "{\"Args\":[\"GetTransfer\",\"${LOCKED}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
if [ "${after}" = "PREPARED" ]; then
  REFUSED=$(( REFUSED + 1 ))
  printf '  \033[0;32mrefused\033[0m  [%-7s] %s\n' "T2.2" \
    "a commit endorsed by one organisation only (still PREPARED)"
else
  fail "[T2.2] a single-org commit changed the transfer to ${after}"
fi

# ---------------------------------------------------------------------------
step "Netting"
# ---------------------------------------------------------------------------
SEQ="$(query '{"Args":["CurrentWindow"]}' | tr -d '"')"
close_args="{\"Args\":[\"CloseWindow\",\"AED\",\"${SEQ}\"]}"
attack "T8.5" "closing a window that is still accruing" \
  "still open" 1 "${close_args}"

# Cover: commit more than the account holds. Everything spendable is already
# locked by the double-spend attempt above, so any obligation is uncovered.
# More than the payer now has free, since the double-spend attempt above
# locked half of it.
ob_args="{\"Args\":[\"SubmitObligation\",\"OB-ADV-${STAMP}\",\"Org2MSP\",\"AED\",\"${AVAIL}\"]}"
attack "T8.1" "committing to a netting window without cover" \
  "cannot commit" 1 "${ob_args}"

# ---------------------------------------------------------------------------
step "Policy and maker-checker"
# ---------------------------------------------------------------------------
# Propose a cap, then try to approve it single-handedly.
policy_args="$(node -e '
process.stdout.write(JSON.stringify({
  Args: ["ProposePolicy", JSON.stringify({
    maxSingleTransfer: { AED: 1000 },
    maxObligation: {},
    approvedMembers: [],
    minSettlementWindowSeconds: 0,
  })],
}));
')"
# sed, not `tr -d`. `tr -d 'payload:"'` deletes every character in that SET,
# which includes a, d, l, o and y — so it quietly eats those hex digits out of
# the ID and leaves a 56-character string that matches no proposal.
PROPOSAL="$(run_invoke 1 "${policy_args}" \
  | grep -o 'payload:"[0-9a-f]*"' | sed 's/^payload:"//; s/"$//' || true)"
sleep "${COMMIT_WAIT}"
if ! echo "${PROPOSAL}" | grep -qE '^[0-9a-f]{64}$'; then
  PROPOSAL="$(query '{"Args":["ListPolicyProposals"]}' \
    | python3 -c 'import sys,json; d=json.load(sys.stdin) or []; print(d[0]["proposalID"] if d else "")' 2>/dev/null || echo "")"
fi
if ! echo "${PROPOSAL}" | grep -qE '^[0-9a-f]{64}$'; then
  fail "could not read a usable proposal ID; got ${PROPOSAL:-<empty>}"
  PROPOSAL=""
fi
if [ -n "${PROPOSAL}" ]; then
  approve_args="{\"Args\":[\"ApprovePolicy\",\"${PROPOSAL}\"]}"
  attack "T10.3.1" "the proposer of a limit change approving it itself" \
    "may not also approve it" 1 "${approve_args}"

  # Now approve properly, and check the cap actually bites.
  ok_invoke 2 "${approve_args}"
  over_args="$(create_transfer_args --id "TP-ADV-OVERCAP-${STAMP}" --sender 500000 --sender-ccy AED \
    --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
  attack "T10.3.4" "a transfer above the agreed per-transfer limit" \
    "exceeds the policy limit" 1 "${over_args}"

  # Put the channel back to unrestricted, or everything after this run is capped.
  reset_args="$(node -e '
process.stdout.write(JSON.stringify({
  Args: ["ProposePolicy", JSON.stringify({
    maxSingleTransfer: {},
    maxObligation: {},
    approvedMembers: [],
    minSettlementWindowSeconds: 0,
  })],
}));
')"
  ok_invoke 1 "${reset_args}"
  RESET_ID="$(query '{"Args":["ListPolicyProposals"]}' \
    | python3 -c 'import sys,json; d=json.load(sys.stdin) or []; print(d[0]["proposalID"] if d else "")' 2>/dev/null || echo "")"
  if [ -n "${RESET_ID}" ]; then
    ok_invoke 2 "{\"Args\":[\"ApprovePolicy\",\"${RESET_ID}\"]}"
    echo "  policy reset to unrestricted"
  else
    fail "could not reset the policy; later runs on this ledger will be capped"
  fi
else
  fail "[T10.3.1] could not create a policy proposal to attack"
  ATTACKS=$(( ATTACKS + 1 ))
fi

# ---------------------------------------------------------------------------
step "Reads that must not be privileged"
# ---------------------------------------------------------------------------
attack_query "T10.2" "asking about a beneficiary nobody screened" \
  "not payable" "{\"Args\":[\"CheckBeneficiary\",\"${UNSCREENED}\"]}"

# ---------------------------------------------------------------------------
step "Conservation, after everything above"
# ---------------------------------------------------------------------------
# The point of running this last: none of the attacks above may have moved
# value, and a fold over every account is how that is checked.
query '{"Args":["ListBalances"]}' | python3 -c '
import sys, json, collections
accounts = json.load(sys.stdin) or []
per = collections.defaultdict(int)
for a in accounts:
    per[a["ccy"]] += a["balance"]
print("  %d accounts folded" % len(accounts))
bad = False
for ccy in sorted(per):
    verdict = "BALANCED" if per[ccy] == 0 else "NOT BALANCED"
    if per[ccy] != 0:
        bad = True
    print("  %s sums to %d   %s" % (ccy, per[ccy], verdict))
sys.exit(1 if bad else 0)
' || fail "the books do not balance after the attacks"

# ---------------------------------------------------------------------------
step "Summary"
# ---------------------------------------------------------------------------
echo "${REFUSED} of ${ATTACKS} attacks were refused, each for the reason it should have been."
if [ "${A_EXIT}" -ne 0 ]; then
  echo "Some attacks were NOT refused, or were refused by the wrong thing. See above."
fi
echo
echo "A_EXIT=${A_EXIT}"
exit "${A_EXIT}"
