#!/usr/bin/env bash
# Three-party multilateral netting against the live network.
#
# demo-netting.sh shows two institutions netting against each other, which is
# bilateral and only ever cancels what happens to flow both ways. The
# interesting case needs three, and this script runs two of them:
#
#   A perfect cycle. Org1 owes Org2, Org2 owes Org3, Org3 owes Org1, all the
#   same amount. Every position nets to zero, so the window settles by moving
#   nothing at all. Bilateral netting cannot find this: no two of those three
#   obligations offset each other.
#
#   A chain. Org1 owes Org2 and Org2 owes Org3 the same amount. Org2's position
#   nets to zero and it drops out entirely — its balance is never written — and
#   the whole window settles as one movement from Org1 straight to Org3.
#
# PlatformOrg (Org3MSP) runs no peer of its own. It does not need one: it signs
# with its own admin identity and addresses Org1's Lite Peer, so the chaincode
# reads Org3MSP from the signature while the endorsement policy is satisfied by
# the two organisations that do run peers.
#
#   ./network/deploy-cc.sh settlement && ./network/add-platform-org.sh \
#     && ./network/demo-netting-multilateral.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"
CCY="AED"
WINDOW_SECONDS=60

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
M_EXIT=0
fail() { echo "FAIL: $*"; M_EXIT=1; }
ok()   { echo "OK: $*"; }

[ -d "${ORG3_ADMIN_MSP}" ] || {
  echo "PlatformOrg's admin MSP is missing at ${ORG3_ADMIN_MSP}"
  echo "Run ./network/add-platform-org.sh first."
  exit 1
}

# as_org <1|2|3> points the CLI at a signing identity. Org3 has no peer of its
# own, so it borrows Org1's Lite Peer as a transport while signing as itself —
# the chaincode reads the MSP ID from the signature, not from the address.
as_org() {
  case "$1" in
    3)
      export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
      export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
      export CORE_PEER_ADDRESS="localhost:7051"
      export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
      export CORE_PEER_TLS_ENABLED=true
      ;;
    *) setGlobals "$1" "${LITE_PEER}" >/dev/null 2>&1 ;;
  esac
}

invoke_fast() {
  local org="$1"; shift
  as_org "${org}"
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" \
    -c "$1" 2>&1 | tail -1
}

invoke() { invoke_fast "$1" "$2"; sleep "${COMMIT_WAIT}"; }

query_raw() {
  local org="$1"; shift
  as_org "${org}"
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

bal() {
  query_raw 1 "{\"Args\":[\"GetBalance\",\"$1\",\"${CCY}\"]}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])'
}

PARTIES=(Org1MSP Org2MSP Org3MSP)

# wait_for_next_window blocks until the window rolls, then reports the new one.
wait_for_next_window() {
  local from="$1"
  while [ "$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')" = "${from}" ]; do
    sleep 5
  done
}

# ---------------------------------------------------------------------------
step "Confirming PlatformOrg can transact while running no peer of its own"
# ---------------------------------------------------------------------------
CUR="$(query_raw 3 '{"Args":["CurrentWindow"]}' | tr -d '"')"
if [ -z "${CUR}" ]; then
  fail "PlatformOrg could not reach the chaincode"
  echo "M_EXIT=1"; exit 1
fi
ok "PlatformOrg read the ledger as ${ORG3_MSP}; current window ${CUR}"

# ---------------------------------------------------------------------------
step "Funding all three institutions in ${CCY}"
# ---------------------------------------------------------------------------
# Seeded one at a time on purpose: every SeedBalance debits issuance:<CCY>,
# one shared key, so firing them together puts them in one block and MVCC
# throws away all but one. That is the hot-key problem this whole exercise is
# about, and it applies to minting too — issuance is still a shared key and
# would need sharding before it sat on any hot path.
for org in 1 2 3; do
  invoke "${org}" "{\"Args\":[\"SeedBalance\",\"${CCY}\",\"20000000\"]}" >/dev/null
done
for p in "${PARTIES[@]}"; do
  amount="$(bal "${p}")"
  echo "${p} ${CCY} ${amount}"
  if [ "${amount}" -le 0 ]; then
    fail "${p} has no ${CCY}; its seed did not commit"
    echo "M_EXIT=1"; exit 1
  fi
done

# ---------------------------------------------------------------------------
step "Settling any window an earlier run left open"
# ---------------------------------------------------------------------------
NOW_SEQ="$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')"
CLEARED=0
for back in $(seq 1 20); do
  past=$(( NOW_SEQ - back ))
  [ "${past}" -lt 0 ] && break
  info="$(query_raw 1 "{\"Args\":[\"GetWindow\",\"${CCY}\",\"${past}\"]}" 2>/dev/null || true)"
  [ -z "${info}" ] && continue
  read -r wstate wobs <<<"$(echo "${info}" | python3 -c '
import sys, json
try:
    w = json.load(sys.stdin)
    print(w["state"], w["obligations"])
except Exception:
    print("UNKNOWN 0")
')"
  if [ "${wstate}" = "OPEN" ] && [ "${wobs}" -gt 0 ]; then
    echo "settling window ${past}, left open with ${wobs} obligations"
    invoke_fast 1 "{\"Args\":[\"CloseWindow\",\"${CCY}\",\"${past}\"]}" >/dev/null 2>&1 || true
    CLEARED=$(( CLEARED + 1 ))
  fi
done
[ "${CLEARED}" -gt 0 ] && sleep "${COMMIT_WAIT}"
echo "cleared ${CLEARED} leftover window(s)"

# ---------------------------------------------------------------------------
# Scenario one: a perfect cycle
# ---------------------------------------------------------------------------
step "Scenario 1 — a cycle of three debts that cancels completely"

# Plain variables rather than an associative array: macOS ships bash 3.2,
# which has no `declare -A`, and this script has to run where it was written.
B1_ORG1="$(bal Org1MSP)"; B1_ORG2="$(bal Org2MSP)"; B1_ORG3="$(bal Org3MSP)"

SLEEP_FOR=$(( WINDOW_SECONDS - ($(date +%s) % WINDOW_SECONDS) ))
echo "sleeping ${SLEEP_FOR}s to the next window boundary"
sleep "${SLEEP_FOR}"
SEQ1="$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')"
echo "window ${SEQ1}: Org1 owes Org2 600000, Org2 owes Org3 600000, Org3 owes Org1 600000"

# All three write different keys — three obligation records and three separate
# exposure records — so all three commit from the same block.
invoke_fast 1 "{\"Args\":[\"SubmitObligation\",\"MC-${STAMP}-1\",\"Org2MSP\",\"${CCY}\",\"600000\"]}" >/dev/null
invoke_fast 2 "{\"Args\":[\"SubmitObligation\",\"MC-${STAMP}-2\",\"Org3MSP\",\"${CCY}\",\"600000\"]}" >/dev/null
invoke_fast 3 "{\"Args\":[\"SubmitObligation\",\"MC-${STAMP}-3\",\"Org1MSP\",\"${CCY}\",\"600000\"]}" >/dev/null
sleep "${COMMIT_WAIT}"

OBS1="$(query_raw 1 "{\"Args\":[\"ListObligations\",\"${CCY}\",\"${SEQ1}\"]}")"
COUNT1="$(echo "${OBS1}" | python3 -c 'import sys,json; print(len(json.load(sys.stdin) or []))')"
echo "${OBS1}" | python3 -c '
import sys, json
for o in json.load(sys.stdin) or []:
    print("  %-10s -> %-10s %10s" % (o["debtor"], o["creditor"], o["amount"]))
'
if [ "${COUNT1}" -eq 3 ]; then
  ok "all three institutions committed into one window, from one block"
else
  fail "${COUNT1} of 3 obligations landed in window ${SEQ1}"
fi

wait_for_next_window "${SEQ1}"
invoke 1 "{\"Args\":[\"CloseWindow\",\"${CCY}\",\"${SEQ1}\"]}" >/dev/null
W1="$(query_raw 1 "{\"Args\":[\"GetWindow\",\"${CCY}\",\"${SEQ1}\"]}")"
echo "${W1}" | python3 -m json.tool

eval "$(echo "${W1}" | python3 -c '
import sys, json
w = json.load(sys.stdin)
for name, key in [("W1_GROSS","grossValue"), ("W1_NET","netValue"),
                  ("W1_POSTINGS","postings"), ("W1_BPS","compressionBps")]:
    print("%s=%s" % (name, w[key]))')"

[ "${W1_NET}" = "0" ] && ok "net 0: the cycle cancelled completely" \
  || fail "net ${W1_NET}, expected 0"
[ "${W1_POSTINGS}" = "0" ] && ok "0 postings: no balance key was written at all" \
  || fail "${W1_POSTINGS} postings, expected 0"
[ "${W1_BPS}" = "10000" ] && ok "compression 10000 bps — 100% of ${W1_GROSS} never moved" \
  || fail "compression ${W1_BPS} bps, expected 10000"

moved_any=0
for pair in "Org1MSP:${B1_ORG1}" "Org2MSP:${B1_ORG2}" "Org3MSP:${B1_ORG3}"; do
  party="${pair%%:*}"; before="${pair##*:}"
  after="$(bal "${party}")"
  if [ "${after}" != "${before}" ]; then
    fail "${party} moved from ${before} to ${after}; a cancelled cycle must move nothing"
    moved_any=1
  fi
done
[ "${moved_any}" -eq 0 ] && ok "every balance is exactly where it started"

echo
echo "Bilateral netting cannot find this. No two of those three obligations"
echo "offset each other — the cancellation only exists across all three."

# ---------------------------------------------------------------------------
# Scenario two: a chain through an intermediary
# ---------------------------------------------------------------------------
step "Scenario 2 — value passes through an institution that never moves"

B2_ORG1="$(bal Org1MSP)"; B2_ORG2="$(bal Org2MSP)"; B2_ORG3="$(bal Org3MSP)"

SLEEP_FOR=$(( WINDOW_SECONDS - ($(date +%s) % WINDOW_SECONDS) ))
echo "sleeping ${SLEEP_FOR}s to the next window boundary"
sleep "${SLEEP_FOR}"
SEQ2="$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')"
echo "window ${SEQ2}: Org1 owes Org2 500000, Org2 owes Org3 500000"

invoke_fast 1 "{\"Args\":[\"SubmitObligation\",\"MC-${STAMP}-4\",\"Org2MSP\",\"${CCY}\",\"500000\"]}" >/dev/null
invoke_fast 2 "{\"Args\":[\"SubmitObligation\",\"MC-${STAMP}-5\",\"Org3MSP\",\"${CCY}\",\"500000\"]}" >/dev/null
sleep "${COMMIT_WAIT}"

wait_for_next_window "${SEQ2}"
invoke 1 "{\"Args\":[\"CloseWindow\",\"${CCY}\",\"${SEQ2}\"]}" >/dev/null
W2="$(query_raw 1 "{\"Args\":[\"GetWindow\",\"${CCY}\",\"${SEQ2}\"]}")"
echo "${W2}" | python3 -m json.tool

eval "$(echo "${W2}" | python3 -c '
import sys, json
w = json.load(sys.stdin)
for name, key in [("W2_GROSS","grossValue"), ("W2_NET","netValue"),
                  ("W2_POSTINGS","postings"), ("W2_OBS","obligations")]:
    print("%s=%s" % (name, w[key]))')"

[ "${W2_POSTINGS}" = "1" ] && ok "one posting settles a two-hop chain" \
  || fail "${W2_POSTINGS} postings, expected 1"

src_after="$(bal Org1MSP)"
mid_after="$(bal Org2MSP)"
dst_after="$(bal Org3MSP)"
echo "Org1MSP ${B2_ORG1} -> ${src_after}  (moved $(( src_after - B2_ORG1 )))"
echo "Org2MSP ${B2_ORG2} -> ${mid_after}  (moved $(( mid_after - B2_ORG2 )))"
echo "Org3MSP ${B2_ORG3} -> ${dst_after}  (moved $(( dst_after - B2_ORG3 )))"

[ "${mid_after}" = "${B2_ORG2}" ] \
  && ok "Org2MSP netted to zero and was never written — value passed straight through it" \
  || fail "Org2MSP moved from ${B2_ORG2} to ${mid_after}"
[ "$(( B2_ORG1 - src_after ))" = "500000" ] \
  && ok "Org1MSP paid 500000" || fail "Org1MSP moved by $(( src_after - B2_ORG1 ))"
[ "$(( dst_after - B2_ORG3 ))" = "500000" ] \
  && ok "Org3MSP received 500000, from an institution it has no obligation with" \
  || fail "Org3MSP moved by $(( dst_after - B2_ORG3 ))"

# ---------------------------------------------------------------------------
step "Conservation across every account on the ledger"
# ---------------------------------------------------------------------------
query_raw 1 '{"Args":["ListBalances"]}' | python3 -c '
import sys, json, collections
accounts = json.load(sys.stdin) or []
per = collections.defaultdict(int)
for a in accounts:
    per[a["ccy"]] += a["balance"]
    print("  %-30s %s  dr=%-14s cr=%-14s bal=%14s" % (
        a["account"], a["ccy"], a["debits"], a["credits"], a["balance"]))
bad = False
for ccy in sorted(per):
    verdict = "BALANCED" if per[ccy] == 0 else "NOT BALANCED"
    if per[ccy] != 0:
        bad = True
    print("  %s sums to %d   %s" % (ccy, per[ccy], verdict))
sys.exit(1 if bad else 0)
' || fail "the books do not balance"

step "Summary"
echo "cycle:  ${W1_GROSS} obligated across 3 institutions, ${W1_NET} settled, ${W1_POSTINGS} postings"
echo "chain:  ${W2_GROSS} obligated across ${W2_OBS} obligations, ${W2_NET} settled, ${W2_POSTINGS} posting"
echo
echo "M_EXIT=${M_EXIT}"
exit "${M_EXIT}"
