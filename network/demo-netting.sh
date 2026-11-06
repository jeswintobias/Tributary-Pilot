#!/usr/bin/env bash
# Run the netting-first settlement path against the live network and check the
# arithmetic against committed state.
#
# What this script is for: the two-phase commit in demo.sh is atomic and it
# serialises on the payer's balance key, so a corridor using it settles at
# roughly one transfer per block. The netting path submits obligations that
# touch no shared key, then settles a whole window with a single residual
# movement. This script proves that happens on DRUNIX rather than only in the
# unit tests.
#
# Everything claimed below is derived from state read back off the ledger. The
# expected net is computed from the obligations the chain actually recorded in
# the window, not from what this script intended to submit, so a submission that
# landed in a different window cannot silently flatter the result.
#
#   ./network/deploy-cc.sh settlement && ./network/demo-netting.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"
CCY="AED"

# Must match NettingWindowSeconds in chaincode/settlement/settlement/pool.go.
# Checked below rather than trusted.
WINDOW_SECONDS=60

configure_platform
require_tools
ensure_upstream
export_fabric_env

LITE_PEER=0
COMMIT_WAIT="${COMMIT_WAIT:-8}"

step() { printf '\n\033[1;36m\xe2\x94\x80\xe2\x94\x80 %s\033[0m\n' "$*"; }

# invoke_fast endorses and orders without waiting for the commit. Submissions do
# not read each other, so there is nothing to wait for between them — and the
# whole point is to get them all inside one window.
invoke_fast() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" \
    -c "$1" 2>&1 | tail -1
}

invoke() {
  local org="$1"; shift
  invoke_fast "${org}" "$1"
  sleep "${COMMIT_WAIT}"
}

query_raw() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

N_EXIT=0
fail() { echo "FAIL: $*"; N_EXIT=1; }
ok()   { echo "OK: $*"; }

# ---------------------------------------------------------------------------
step "Checking the window length the chaincode is actually using"
# ---------------------------------------------------------------------------
# CurrentWindow is derived from the transaction timestamp, so the window length
# can be recovered from the chain rather than assumed from this script.
CUR="$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')"
NOW="$(date +%s)"
DERIVED=$(( NOW / WINDOW_SECONDS ))
echo "chaincode reports current window ${CUR}; ${WINDOW_SECONDS}s windows would give ${DERIVED}"
if [ "${CUR}" != "${DERIVED}" ]; then
  fail "window length mismatch: this script assumes ${WINDOW_SECONDS}s but the chaincode disagrees"
  echo "N_EXIT=${N_EXIT}"
  exit 1
fi
ok "window length confirmed at ${WINDOW_SECONDS}s"

# ---------------------------------------------------------------------------
step "Funding both institutions in ${CCY} (Phase 0 scaffolding)"
# ---------------------------------------------------------------------------
invoke_fast 1 "{\"Args\":[\"SeedBalance\",\"${CCY}\",\"50000000\"]}"
invoke 2 "{\"Args\":[\"SeedBalance\",\"${CCY}\",\"50000000\"]}"

ORG1_BEFORE="$(query_raw 1 "{\"Args\":[\"GetBalance\",\"Org1MSP\",\"${CCY}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])')"
ORG2_BEFORE="$(query_raw 1 "{\"Args\":[\"GetBalance\",\"Org2MSP\",\"${CCY}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])')"
echo "Org1MSP ${CCY} ${ORG1_BEFORE}"
echo "Org2MSP ${CCY} ${ORG2_BEFORE}"

# ---------------------------------------------------------------------------
step "Waiting for a fresh netting window so every obligation lands in one"
# ---------------------------------------------------------------------------
# A submission lands in the window its own timestamp names, so starting just
# after a boundary leaves the most room before the window rolls.
SLEEP_FOR=$(( WINDOW_SECONDS - ($(date +%s) % WINDOW_SECONDS) ))
echo "sleeping ${SLEEP_FOR}s to the next window boundary"
sleep "${SLEEP_FOR}"
SEQ="$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')"
ok "submitting into window ${SEQ}"

# ---------------------------------------------------------------------------
step "Submitting obligations in both directions, touching no shared balance"
# ---------------------------------------------------------------------------
# Four payments out and two back: gross 1,800,000 fils, of which only the
# residue should ever move.
invoke_fast 1 "{\"Args\":[\"SubmitObligation\",\"OB-${STAMP}-A\",\"Org2MSP\",\"${CCY}\",\"400000\"]}"
invoke_fast 1 "{\"Args\":[\"SubmitObligation\",\"OB-${STAMP}-B\",\"Org2MSP\",\"${CCY}\",\"300000\"]}"
invoke_fast 1 "{\"Args\":[\"SubmitObligation\",\"OB-${STAMP}-C\",\"Org2MSP\",\"${CCY}\",\"250000\"]}"
invoke_fast 1 "{\"Args\":[\"SubmitObligation\",\"OB-${STAMP}-D\",\"Org2MSP\",\"${CCY}\",\"150000\"]}"
invoke_fast 2 "{\"Args\":[\"SubmitObligation\",\"OB-${STAMP}-E\",\"Org1MSP\",\"${CCY}\",\"500000\"]}"
invoke_fast 2 "{\"Args\":[\"SubmitObligation\",\"OB-${STAMP}-F\",\"Org1MSP\",\"${CCY}\",\"200000\"]}"
sleep "${COMMIT_WAIT}"

# ---------------------------------------------------------------------------
step "Reading the obligations the chain recorded in window ${SEQ}"
# ---------------------------------------------------------------------------
OBS_JSON="$(query_raw 1 "{\"Args\":[\"ListObligations\",\"${CCY}\",\"${SEQ}\"]}")"
echo "${OBS_JSON}" | python3 -c '
import sys, json
obs = json.load(sys.stdin) or []
for o in obs:
    print(f"  {o[\"obligationID\"]:24} {o[\"debtor\"]:10} -> {o[\"creditor\"]:10} {o[\"amount\"]:>10}")
print(f"  {len(obs)} obligations in the window")
'
OB_COUNT="$(echo "${OBS_JSON}" | python3 -c 'import sys,json; print(len(json.load(sys.stdin) or []))')"
if [ "${OB_COUNT}" -ne 6 ]; then
  echo "NOTE: ${OB_COUNT} of 6 obligations landed in window ${SEQ}; the rest crossed"
  echo "      the boundary into the next one. The checks below use what is actually"
  echo "      in this window, so the arithmetic still holds."
fi

# Expected net positions, derived from committed obligations.
read -r EXP_GROSS EXP_NET EXP_ORG1 EXP_ORG2 <<<"$(echo "${OBS_JSON}" | python3 -c '
import sys, json
obs = json.load(sys.stdin) or []
net = {}
gross = 0
for o in obs:
    a = int(o["amount"]); gross += a
    net[o["debtor"]] = net.get(o["debtor"], 0) - a
    net[o["creditor"]] = net.get(o["creditor"], 0) + a
# The settled net value is the sum of the positive positions: what the net
# creditors receive, which is what has to cross the pool.
settled = sum(v for v in net.values() if v > 0)
print(gross, settled, net.get("Org1MSP", 0), net.get("Org2MSP", 0))
')"
echo "derived from the ledger: gross ${EXP_GROSS}, net to settle ${EXP_NET}"
echo "derived net positions: Org1MSP ${EXP_ORG1}, Org2MSP ${EXP_ORG2}"

# ---------------------------------------------------------------------------
step "Projecting the window before closing it"
# ---------------------------------------------------------------------------
PROJ="$(query_raw 1 "{\"Args\":[\"GetWindow\",\"${CCY}\",\"${SEQ}\"]}")"
echo "${PROJ}"
PROJ_STATE="$(echo "${PROJ}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
if [ "${PROJ_STATE}" != "OPEN" ]; then
  fail "window ${SEQ} reports ${PROJ_STATE} before it was closed"
else
  ok "window reports OPEN before settlement"
fi

# ---------------------------------------------------------------------------
step "Refusing to close a window that is still open"
# ---------------------------------------------------------------------------
# This is the guard that makes CloseWindow's range query safe from phantom
# reads, so it is worth proving it actually refuses.
CUR_NOW="$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')"
if [ "${CUR_NOW}" = "${SEQ}" ]; then
  early="$(invoke_fast 1 "{\"Args\":[\"CloseWindow\",\"${CCY}\",\"${SEQ}\"]}" || true)"
  if echo "${early}" | grep -q "still open"; then
    ok "closing the current window was refused: $(echo "${early}" | tail -c 120)"
  else
    fail "closing the current window was not refused: ${early}"
  fi
else
  echo "window already rolled to ${CUR_NOW}; skipping the still-open check"
fi

# ---------------------------------------------------------------------------
step "Waiting for window ${SEQ} to fall into the past, then closing it"
# ---------------------------------------------------------------------------
while [ "$(query_raw 1 '{"Args":["CurrentWindow"]}' | tr -d '"')" = "${SEQ}" ]; do
  sleep 5
done
invoke 1 "{\"Args\":[\"CloseWindow\",\"${CCY}\",\"${SEQ}\"]}"

SETTLED="$(query_raw 1 "{\"Args\":[\"GetWindow\",\"${CCY}\",\"${SEQ}\"]}")"
echo "${SETTLED}" | python3 -m json.tool

# ---------------------------------------------------------------------------
step "Checking the settlement against the obligations"
# ---------------------------------------------------------------------------
eval "$(echo "${SETTLED}" | python3 -c '
import sys, json
w = json.load(sys.stdin)
print(f"W_STATE={w[\"state\"]}")
print(f"W_OBS={w[\"obligations\"]}")
print(f"W_GROSS={w[\"grossValue\"]}")
print(f"W_NET={w[\"netValue\"]}")
print(f"W_POSTINGS={w[\"postings\"]}")
print(f"W_BPS={w[\"compressionBps\"]}")
print(f"W_CLOSEDBY={w[\"closedBy\"]}")
')"

[ "${W_STATE}" = "SETTLED" ] && ok "window state SETTLED" || fail "window state ${W_STATE}"
[ "${W_OBS}" = "${OB_COUNT}" ] && ok "window counted ${W_OBS} obligations" \
  || fail "window counted ${W_OBS}, ledger holds ${OB_COUNT}"
[ "${W_GROSS}" = "${EXP_GROSS}" ] && ok "gross ${W_GROSS} matches the obligations" \
  || fail "gross ${W_GROSS}, obligations sum to ${EXP_GROSS}"
[ "${W_NET}" = "${EXP_NET}" ] && ok "net ${W_NET} matches the derived net positions" \
  || fail "net ${W_NET}, derived ${EXP_NET}"

# ---------------------------------------------------------------------------
step "Checking each balance moved by exactly its net position"
# ---------------------------------------------------------------------------
# This is the check that says netting was CORRECT, not merely balanced: a wrong
# amount applied to both sides of a posting leaves the books balanced while
# settling the wrong figure.
ORG1_AFTER="$(query_raw 1 "{\"Args\":[\"GetBalance\",\"Org1MSP\",\"${CCY}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])')"
ORG2_AFTER="$(query_raw 1 "{\"Args\":[\"GetBalance\",\"Org2MSP\",\"${CCY}\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["balance"])')"

ORG1_MOVED=$(( ORG1_AFTER - ORG1_BEFORE ))
ORG2_MOVED=$(( ORG2_AFTER - ORG2_BEFORE ))
echo "Org1MSP ${ORG1_BEFORE} -> ${ORG1_AFTER}  (moved ${ORG1_MOVED}, net position ${EXP_ORG1})"
echo "Org2MSP ${ORG2_BEFORE} -> ${ORG2_AFTER}  (moved ${ORG2_MOVED}, net position ${EXP_ORG2})"
[ "${ORG1_MOVED}" = "${EXP_ORG1}" ] && ok "Org1MSP moved by exactly its net position" \
  || fail "Org1MSP moved ${ORG1_MOVED}, net position is ${EXP_ORG1}"
[ "${ORG2_MOVED}" = "${EXP_ORG2}" ] && ok "Org2MSP moved by exactly its net position" \
  || fail "Org2MSP moved ${ORG2_MOVED}, net position is ${EXP_ORG2}"

# ---------------------------------------------------------------------------
step "Checking the reservations were released"
# ---------------------------------------------------------------------------
for org in Org1MSP Org2MSP; do
  outstanding="$(query_raw 1 "{\"Args\":[\"GetExposure\",\"${org}\",\"${CCY}\"]}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["outstanding"])')"
  if [ "${outstanding}" = "0" ]; then
    ok "${org} has no outstanding netting exposure"
  else
    fail "${org} still has ${outstanding} reserved after settlement"
  fi
done

# ---------------------------------------------------------------------------
step "Refusing a replayed close"
# ---------------------------------------------------------------------------
replay="$(invoke_fast 2 "{\"Args\":[\"CloseWindow\",\"${CCY}\",\"${SEQ}\"]}" || true)"
if echo "${replay}" | grep -q "already settled"; then
  ok "replayed close refused"
else
  fail "replayed close was not refused: ${replay}"
fi

# ---------------------------------------------------------------------------
step "Conservation across every account on the ledger"
# ---------------------------------------------------------------------------
query_raw 1 '{"Args":["ListBalances"]}' | python3 -c '
import sys, json, collections
accounts = json.load(sys.stdin) or []
per = collections.defaultdict(int)
for a in accounts:
    per[a["ccy"]] += a["balance"]
    print(f"  {a[\"account\"]:<30} {a[\"ccy\"]}  dr={a[\"debits\"]:<14} cr={a[\"credits\"]:<14} bal={a[\"balance\"]:>14}")
bad = False
for ccy in sorted(per):
    verdict = "BALANCED" if per[ccy] == 0 else "NOT BALANCED"
    if per[ccy] != 0:
        bad = True
    print(f"  {ccy} sums to {per[ccy]}   {verdict}")
sys.exit(1 if bad else 0)
' || fail "the books do not balance"

# ---------------------------------------------------------------------------
step "Write amplification on the shared balance keys"
# ---------------------------------------------------------------------------
UNNETTED=$(( 2 * W_OBS ))
echo "netted:   ${W_POSTINGS} journal postings to settle ${W_OBS} obligations"
echo "unnetted: ${UNNETTED} postings, two per obligation, each one serialising on a balance key"
echo "value:    gross ${W_GROSS} obligated, net ${W_NET} settled, compression ${W_BPS} bps"
echo "closed by ${W_CLOSEDBY}"
if [ "${W_POSTINGS}" -lt "${UNNETTED}" ]; then
  ok "netting reduced postings from ${UNNETTED} to ${W_POSTINGS}"
else
  fail "netting did not reduce postings: ${W_POSTINGS} vs ${UNNETTED}"
fi

echo
echo "N_EXIT=${N_EXIT}"
exit "${N_EXIT}"
