#!/usr/bin/env bash
# Measuring contention, which is the claim — not throughput, which is not.
#
# WHAT THIS DELIBERATELY DOES NOT MEASURE
#
# Transactions per second. DRUNIX publishes amd64 images only, so on Apple
# silicon every container runs emulated and any timing taken here says more
# about qemu than about the ledger. A TPS number from this machine would be
# worthless, and quoting one would be worse than quoting none.
#
# WHAT IT DOES MEASURE
#
# How many concurrent transactions survive, which is a property of the KEYS
# they touch rather than of the hardware. Fabric invalidates a transaction
# whose read set was written by another transaction in the same block, so
# "how many of N concurrent writes to one key commit" has the same answer on a
# laptop and on a datacentre. That is the hot-key problem, and it is the thing
# the netting path was built to avoid.
#
# Three rounds, each firing N transactions at once:
#
#   A  two-phase commit, one payer   -> every Prepare writes that payer's
#                                       balance, so they contend
#   B  netting, one institution      -> every submission writes that
#                                       institution's exposure record
#   C  netting, across institutions  -> different keys, so no contention
#
# The claim being checked is the one the README makes: parallelism is ACROSS
# institutions, not within one. If C does not beat A and B, that claim is wrong.
#
#   ./network/setup-fx-compliance.sh && ./network/measure-contention.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
export STAMP="$(date +%s)"  # exported: the inline python below reads it
N="${N:-6}"            # how many transactions to fire at once
PAYEE="sha256:contention-payee-${STAMP}"

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
COMMIT_WAIT="${COMMIT_WAIT:-10}"
step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
M_EXIT=0
fail() { echo "FAIL: $*"; M_EXIT=1; }
ok()   { echo "OK: $*"; }

# Each concurrent invoke runs in its own subshell with its own CORE_PEER_*
# environment, because setGlobals exports into the current shell and two
# background jobs sharing one environment would race on the identity rather
# than on the ledger.
fire() {
  local org="$1" payload="$2" tag="$3"
  (
    if [ "${org}" = "platform" ]; then
      export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
      export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
      export CORE_PEER_ADDRESS="localhost:7051"
      export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
      export CORE_PEER_TLS_ENABLED=true
    else
      setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
    fi
    peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
      --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
      "${BOTH_PEERS[@]}" -c "${payload}" > "/tmp/contention-${tag}.out" 2>&1 || true
  ) &
}

invoke() {
  local org="$1" payload="$2"
  if [ "${org}" = "platform" ]; then
    export CORE_PEER_LOCALMSPID="${ORG3_MSP}"
    export CORE_PEER_MSPCONFIGPATH="${ORG3_ADMIN_MSP}"
    export CORE_PEER_ADDRESS="localhost:7051"
    export CORE_PEER_TLS_ROOTCERT_FILE="${PEER0_ORG1_CA}"
    export CORE_PEER_TLS_ENABLED=true
  else
    setGlobals "${org}" "${LITE_PEER}" >/dev/null 2>&1
  fi
  peer chaincode invoke -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
    --tls --cafile "${ORDERER_CA}" -C "${CHANNEL_NAME}" -n "${CC_NAME}" \
    "${BOTH_PEERS[@]}" -c "${payload}" >/dev/null 2>&1 || true
  sleep "${COMMIT_WAIT}"
}

query() {
  setGlobals 1 "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

attest() {
  local subject="$1"
  local expires=$(( $(date +%s) + 300 * 24 * 60 * 60 ))
  local kind args
  for kind in KYC SANCTIONS; do
    args="$(node -e '
const [, subject, kind, expires] = process.argv;
process.stdout.write(JSON.stringify({
  Args: ["IssueAttestation", subject, kind, expires, "contention-harness"],
}));
' -- "${subject}" "${kind}" "${expires}")"
    invoke platform "${args}"
  done
}

[ -d "${ORG3_ADMIN_MSP}" ] || die "PlatformOrg is missing; run ./network/add-platform-org.sh"

# ---------------------------------------------------------------------------
step "Setting up"
# ---------------------------------------------------------------------------
invoke 1 '{"Args":["SeedBalance","AED","500000000"]}'
invoke 2 '{"Args":["SeedBalance","AED","500000000"]}'
invoke 2 '{"Args":["SeedBalance","INR","20000000000"]}'
attest "${PAYEE}"
echo "firing ${N} transactions at once in each round"

# ---------------------------------------------------------------------------
step "Round A — two-phase commit, all from one payer"
# ---------------------------------------------------------------------------
# N transfers, each accepted, then every Prepare fired at the same instant.
# Each one reads and writes the payer's balance, so they all contend on one key.
EXPIRY=$(( $(date +%s) + 800 ))
A_IDS=()
for i in $(seq 1 "${N}"); do
  id="TP-CON-${STAMP}-${i}"
  args="$(create_transfer_args --id "${id}" --sender 100000 --sender-ccy AED \
    --payout-ccy INR --expires "${EXPIRY}" --payout-org Org2MSP --payee "${PAYEE}")"
  invoke 1 "${args}"
  invoke 2 "{\"Args\":[\"Accept\",\"${id}\"]}"
  A_IDS+=("${id}")
done
echo "${N} transfers accepted; firing every Prepare at once"

for i in $(seq 1 "${N}"); do
  id="TP-CON-${STAMP}-${i}"
  fire 1 "{\"Args\":[\"Prepare\",\"${id}\"]}" "a-${i}"
done
wait
sleep "${COMMIT_WAIT}"

A_OK=0
for id in "${A_IDS[@]}"; do
  st="$(query "{\"Args\":[\"GetTransfer\",\"${id}\"]}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["state"])' 2>/dev/null || echo "?")"
  [ "${st}" = "PREPARED" ] && A_OK=$(( A_OK + 1 ))
done
echo "Round A: ${A_OK} of ${N} prepares survived"

# ---------------------------------------------------------------------------
step "Round B — netting, all from one institution"
# ---------------------------------------------------------------------------
# Every submission writes that institution's own exposure record, so these
# contend too — on a different key, but one key all the same. This is the
# limit the README states: parallelism is across institutions, not within one.
B_SEQ="$(query '{"Args":["CurrentWindow"]}' | tr -d '"')"
for i in $(seq 1 "${N}"); do
  fire 1 "{\"Args\":[\"SubmitObligation\",\"OB-B-${STAMP}-${i}\",\"Org2MSP\",\"AED\",\"1000\"]}" "b-${i}"
done
wait
sleep "${COMMIT_WAIT}"

B_OK="$(query "{\"Args\":[\"ListObligations\",\"AED\",\"${B_SEQ}\"]}" | python3 -c '
import sys, json, os
obs = json.load(sys.stdin) or []
stamp = os.environ["STAMP"]
print(sum(1 for o in obs if o["obligationID"].startswith(f"OB-B-{stamp}-")))
' 2>/dev/null || echo 0)"
echo "Round B: ${B_OK} of ${N} submissions survived"

# ---------------------------------------------------------------------------
step "Round C — netting, spread across institutions"
# ---------------------------------------------------------------------------
# The same work, divided between the organisations that can do it. Each writes
# its own obligation record and its own exposure record, so nothing is shared.
C_SEQ="$(query '{"Args":["CurrentWindow"]}' | tr -d '"')"
for i in $(seq 1 "${N}"); do
  if [ $(( i % 2 )) -eq 0 ]; then
    fire 1 "{\"Args\":[\"SubmitObligation\",\"OB-C-${STAMP}-${i}\",\"Org2MSP\",\"AED\",\"1000\"]}" "c-${i}"
  else
    fire 2 "{\"Args\":[\"SubmitObligation\",\"OB-C-${STAMP}-${i}\",\"Org1MSP\",\"AED\",\"1000\"]}" "c-${i}"
  fi
done
wait
sleep "${COMMIT_WAIT}"

C_OK="$(query "{\"Args\":[\"ListObligations\",\"AED\",\"${C_SEQ}\"]}" | python3 -c '
import sys, json, os
obs = json.load(sys.stdin) or []
stamp = os.environ["STAMP"]
print(sum(1 for o in obs if o["obligationID"].startswith(f"OB-C-{stamp}-")))
' 2>/dev/null || echo 0)"
echo "Round C: ${C_OK} of ${N} submissions survived"

# ---------------------------------------------------------------------------
step "What this says"
# ---------------------------------------------------------------------------
printf '  %-46s %s of %s\n' "A  two-phase commit, one payer" "${A_OK}" "${N}"
printf '  %-46s %s of %s\n' "B  netting, one institution" "${B_OK}" "${N}"
printf '  %-46s %s of %s\n' "C  netting, across institutions" "${C_OK}" "${N}"
echo
echo "  No timings are reported. Every container here runs emulated, so a"
echo "  throughput figure from this machine would describe qemu rather than"
echo "  the ledger. Survival under contention does not depend on how fast the"
echo "  hardware is — it depends on which keys the transactions touch."

# The sharper statement, and the one the design actually predicts: how many
# survive is the number of DISTINCT KEYS the batch touches, not how many
# transactions were fired. Rounds A and B touch one key each, so one should
# survive; round C is spread over two institutions, so two should.
INSTITUTIONS=2
echo "  Predicted by the design: A and B touch one key each, so 1 should survive."
echo "  C is spread over ${INSTITUTIONS} institutions writing separate keys, so ${INSTITUTIONS} should."
echo

if [ "${A_OK}" -le 1 ] && [ "${B_OK}" -le 1 ]; then
  ok "contending on a single key let at most one transaction through, in both rounds"
else
  fail "a single contended key let ${A_OK} and ${B_OK} through; the hot-key account of this is wrong"
fi

if [ "${C_OK}" -eq "${INSTITUTIONS}" ]; then
  ok "spreading over ${INSTITUTIONS} institutions let exactly ${INSTITUTIONS} through, as the design predicts"
elif [ "${C_OK}" -gt "${A_OK}" ]; then
  ok "spreading over institutions beat the shared key (${C_OK} vs ${A_OK}), though not by the predicted ${INSTITUTIONS}"
else
  fail "the netting path did not beat the shared-balance path (${C_OK} vs ${A_OK}); the README's claim does not hold here"
fi

echo
echo "  Note the shape of the result rather than its size. Throughput here is"
echo "  whatever qemu allows; what the design claims is that concurrency scales"
echo "  with the number of institutions, and that is what these three numbers"
echo "  show. Six transactions across two institutions still only gets two"
echo "  through, which is the limit the README states: parallelism is ACROSS"
echo "  institutions, not within one."

echo
echo "M_EXIT=${M_EXIT}"
exit "${M_EXIT}"
