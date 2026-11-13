#!/usr/bin/env bash
# The Phase C pipeline, end to end against the live network.
#
#   ledger -> projector -> read model + outbox -> payout worker -> payouts/DLQ
#
# What this is for. The chaincode demos prove the ledger is right. They say
# nothing about the service layer around it, which is where the interesting
# failures live: an event missed while a process was down, an event applied
# twice after a restart, a settled transfer that never gets paid, one broken
# payout blocking every payout behind it. This exercises all four.
#
# It settles two transfers. One has a beneficiary the simulated rail accepts;
# the other has one it always rejects, so the dead-letter path is demonstrated
# rather than described.
#
#   ./network/deploy-cc.sh settlement && ./network/demo-pipeline.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
STAMP="$(date +%s)"
PROJECTOR_DIR="${REPO_DIR}/services/projector"

GOOD_PAYEE="sha256:pipeline-good-${STAMP}"
BAD_PAYEE="sha256:pipeline-closed-account"

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
P_EXIT=0
fail() { echo "FAIL: $*"; P_EXIT=1; }
ok()   { echo "OK: $*"; }

invoke() {
  local org="$1"; shift
  setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" \
    -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" -c "$1" 2>&1 | tail -1
  sleep "${COMMIT_WAIT}"
}

# dbq runs one read-only SQL query against the projector's store.
dbq() {
  node -e '
const { openDb } = await import(process.argv[1]);
const db = openDb();
const rows = db.prepare(process.argv[2]).all();
console.log(rows.map((r) => Object.values(r).join("|")).join("\n"));
db.close();
' --input-type=module "${PROJECTOR_DIR}/src/db.js" "$1" 2>/dev/null
}

settle_transfer() {
  local id="$1" payee="$2"
  local expiry=$(( $(date +%s) + 600 ))
  invoke 1 "{\"Args\":[\"CreateTransfer\",\"${id}\",\"220000\",\"AED\",\"3000000\",\"INR\",\"quote-${id}\",\"${expiry}\",\"Org2MSP\",\"${payee}\"]}" >/dev/null
  invoke 2 "{\"Args\":[\"Accept\",\"${id}\"]}" >/dev/null
  invoke 1 "{\"Args\":[\"Prepare\",\"${id}\"]}" >/dev/null
  invoke 2 "{\"Args\":[\"Commit\",\"${id}\"]}" >/dev/null
}

# ---------------------------------------------------------------------------
step "Funding, so the two-phase commit has something to move"
# ---------------------------------------------------------------------------
invoke 1 '{"Args":["SeedBalance","AED","10000000"]}' >/dev/null
invoke 2 '{"Args":["SeedBalance","INR","100000000"]}' >/dev/null
ok "balances seeded"

# ---------------------------------------------------------------------------
step "Settling two transfers on the ledger"
# ---------------------------------------------------------------------------
GOOD="TP-PIPE-OK-${STAMP}"
BAD="TP-PIPE-DEAD-${STAMP}"
echo "${GOOD} pays ${GOOD_PAYEE} — the rail will accept this one"
settle_transfer "${GOOD}" "${GOOD_PAYEE}"
echo "${BAD} pays ${BAD_PAYEE} — the rail will reject this one every time"
settle_transfer "${BAD}" "${BAD_PAYEE}"

for id in "${GOOD}" "${BAD}"; do
  state="$(setGlobals 1 "${LITE_PEER}" >/dev/null 2>&1; peer chaincode query -C "${CHANNEL_NAME}" \
    -n "${CC_NAME}" -c "{\"Args\":[\"GetTransfer\",\"${id}\"]}" 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])')"
  [ "${state}" = "SETTLED" ] && ok "${id} is SETTLED on the ledger" \
    || fail "${id} is ${state}, expected SETTLED"
done

# ---------------------------------------------------------------------------
step "Running the projector until it catches up"
# ---------------------------------------------------------------------------
cd "${PROJECTOR_DIR}"
BEFORE_BLOCK="$(dbq 'SELECT next_block FROM checkpoint')"
echo "checkpoint before: block ${BEFORE_BLOCK}"
PROJECTOR_IDLE_EXIT_MS=12000 npm run --silent projector 2>&1 | grep -vE "^$" | tail -20
AFTER_BLOCK="$(dbq 'SELECT next_block FROM checkpoint')"
echo "checkpoint after:  block ${AFTER_BLOCK}"

for id in "${GOOD}" "${BAD}"; do
  state="$(dbq "SELECT state FROM transfers WHERE transfer_id = '${id}'")"
  [ "${state}" = "SETTLED" ] && ok "${id} is SETTLED in the read model too" \
    || fail "${id} reads as '${state}' in the read model"
done

QUEUED="$(dbq 'SELECT COUNT(*) FROM outbox')"
[ "${QUEUED}" = "2" ] && ok "two payouts queued, one per settled transfer" \
  || fail "${QUEUED} payouts queued, expected 2"

# ---------------------------------------------------------------------------
step "Restarting the projector: the replay must change nothing"
# ---------------------------------------------------------------------------
# This is the property the checkpoint and the dedupe table exist for. Running
# again re-reads the checkpoint block from the peer; nothing may be applied a
# second time and no second payout may appear.
APPLIED_BEFORE="$(dbq 'SELECT events_applied FROM checkpoint')"
PROJECTOR_IDLE_EXIT_MS=10000 npm run --silent projector > /tmp/projector-replay.log 2>&1 || true
APPLIED_AFTER="$(dbq 'SELECT events_applied FROM checkpoint')"
SKIPPED="$(grep -c 'skipped' /tmp/projector-replay.log || true)"

echo "events applied before the restart: ${APPLIED_BEFORE}, after: ${APPLIED_AFTER}"
echo "events re-delivered and skipped on the replay: ${SKIPPED}"
[ "${APPLIED_BEFORE}" = "${APPLIED_AFTER}" ] \
  && ok "the replay applied nothing twice" \
  || fail "applied count moved from ${APPLIED_BEFORE} to ${APPLIED_AFTER} on a replay"

QUEUED_AFTER="$(dbq 'SELECT COUNT(*) FROM outbox')"
[ "${QUEUED_AFTER}" = "2" ] \
  && ok "still two payouts queued, not four" \
  || fail "${QUEUED_AFTER} payouts queued after the replay, expected 2"

# ---------------------------------------------------------------------------
step "Running the payout worker"
# ---------------------------------------------------------------------------
# The bad payee is rejected by the simulated rail on every attempt, so it
# exhausts its retries and is dead-lettered. The good one must go through
# regardless — that is the point of dead-lettering rather than blocking.
export PAYOUT_FAIL_REFS="${BAD_PAYEE}"
WORKER_IDLE_EXIT_MS=4000 timeout 240 npm run --silent worker 2>&1 | tail -25 || true

PAID="$(dbq "SELECT rail_ref FROM payouts WHERE transfer_id = '${GOOD}' AND state = 'PAID'")"
[ -n "${PAID}" ] && ok "${GOOD} paid, rail reference ${PAID}" \
  || fail "${GOOD} was not paid"

DEAD="$(dbq "SELECT attempts FROM dead_letters WHERE dedupe_key = 'payout:${BAD}'")"
[ -n "${DEAD}" ] && ok "${BAD} dead-lettered after ${DEAD} attempts, without blocking the other" \
  || fail "${BAD} did not reach the dead-letter table"

REMAINING="$(dbq 'SELECT COUNT(*) FROM outbox')"
[ "${REMAINING}" = "0" ] && ok "the queue is empty: nothing is stuck" \
  || fail "${REMAINING} job(s) still queued"

# ---------------------------------------------------------------------------
step "The read model"
# ---------------------------------------------------------------------------
npm run --silent show

echo
echo "P_EXIT=${P_EXIT}"
exit "${P_EXIT}"
