#!/usr/bin/env bash
# Emit ISO 20022 messages for every transfer on the live ledger.
#
# The ledger settles payments. The rest of the industry describes payments in
# ISO 20022, so a corridor that cannot produce a pacs.008 needs a translation
# layer written by whoever adopts it — and the translation is where meaning
# gets lost. This produces the standard shapes directly from ledger state.
#
#   ./network/demo-iso20022.sh [--out ./iso-messages]
#
# What it checks, rather than just printing: that every message is
# well-formed XML, that the amounts in it match the ledger's minor units
# exactly, and that no personal-data field was invented along the way.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

CC_NAME="${CC_NAME:-settlement}"
OUT_DIR=""
[ "${1:-}" = "--out" ] && OUT_DIR="${2:-}"
ISO_DIR="${REPO_DIR}/services/iso20022"

configure_platform
require_tools
ensure_upstream
export_fabric_env

cd "${TEST_NETWORK_DIR}"
set +u
# shellcheck disable=SC1091
source ./scripts/envVar.sh

LITE_PEER=0
step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
I_EXIT=0
fail() { echo "FAIL: $*"; I_EXIT=1; }
ok()   { echo "OK: $*"; }

query() {
  setGlobals 1 "${LITE_PEER}" >/dev/null 2>&1
  peer chaincode query -C "${CHANNEL_NAME}" -n "${CC_NAME}" -c "$1" 2>/dev/null
}

# ---------------------------------------------------------------------------
step "Reading the ledger"
# ---------------------------------------------------------------------------
query '{"Args":["ListTransfers"]}' > /tmp/iso-transfers.json
COUNT="$(python3 -c 'import sys,json; print(len(json.load(open("/tmp/iso-transfers.json")) or []))' 2>/dev/null || echo 0)"
if [ "${COUNT}" = "0" ]; then
  fail "no transfers on the ledger; run ./network/demo.sh first"
  echo "I_EXIT=1"; exit 1
fi
echo "${COUNT} transfers"
python3 -c '
import json, collections
ts = json.load(open("/tmp/iso-transfers.json"))
for state, n in sorted(collections.Counter(t["state"] for t in ts).items()):
    print("  %-10s %d" % (state, n))
'

# ---------------------------------------------------------------------------
step "Mapping each transfer to its messages"
# ---------------------------------------------------------------------------
EMITTED=0
CHECKED=0
for id in $(python3 -c '
import json
for t in json.load(open("/tmp/iso-transfers.json")):
    print(t["transferID"])
'); do
  query "{\"Args\":[\"GetTransfer\",\"${id}\"]}" > /tmp/iso-one.json
  if [ ! -s /tmp/iso-one.json ]; then
    fail "could not read ${id} back"
    continue
  fi

  if [ -n "${OUT_DIR}" ]; then
    node "${ISO_DIR}/src/cli.js" --out "${OUT_DIR}" < /tmp/iso-one.json 2>/dev/null \
      || { fail "mapping ${id}"; continue; }
  fi

  # Check the messages rather than only producing them: well-formed, amounts
  # matching the ledger exactly, and no personal-data element invented.
  if node "${ISO_DIR}/src/cli.js" < /tmp/iso-one.json > /tmp/iso-out.xml 2>/dev/null; then
    n="$(python3 - <<'PY'
import json, re, sys
from xml.dom.minidom import parseString

transfer = json.load(open("/tmp/iso-one.json"))
raw = open("/tmp/iso-out.xml").read()
docs = re.findall(r'<\?xml[\s\S]*?</Document>', raw)
if not docs:
    print("0 no-documents")
    sys.exit(0)

def minor_to_decimal(minor, ccy):
    digits = 0 if ccy in ("JPY", "KRW") else (3 if ccy in ("KWD", "BHD", "OMR") else 2)
    text = str(minor)
    if digits == 0:
        return text
    padded = text.rjust(digits + 1, "0")
    return padded[:-digits] + "." + padded[-digits:]

problems = []
for doc in docs:
    try:
        parseString(doc)          # well-formed, or this raises
    except Exception as e:
        problems.append("not well-formed: %s" % e)
        continue
    for forbidden in ("<Nm>", "<PstlAdr>", "<BirthDt>", "<EmailAdr>"):
        if forbidden in doc:
            problems.append("carries %s" % forbidden)
    want = minor_to_decimal(transfer["payoutLegAmount"], transfer["payoutCcy"])
    if "IntrBkSttlmAmt" in doc and want not in doc:
        problems.append("settlement amount is not %s" % want)

print("%d %s" % (len(docs), ";".join(problems) if problems else "clean"))
PY
)"
    docs="${n%% *}"
    verdict="${n#* }"
    if [ "${verdict}" = "clean" ]; then
      EMITTED=$(( EMITTED + docs ))
      CHECKED=$(( CHECKED + 1 ))
    else
      fail "${id}: ${verdict}"
    fi
  else
    fail "mapping ${id}"
  fi
done

ok "${EMITTED} messages from ${CHECKED} transfers: well-formed, amounts exact, no personal data"

# ---------------------------------------------------------------------------
step "One of them, in full"
# ---------------------------------------------------------------------------
SAMPLE="$(python3 -c '
import json
ts = json.load(open("/tmp/iso-transfers.json"))
done = [t["transferID"] for t in ts if t["state"] == "SETTLED"]
print(done[0] if done else ts[0]["transferID"])
')"
echo "${SAMPLE}:"
query "{\"Args\":[\"GetTransfer\",\"${SAMPLE}\"]}" > /tmp/iso-one.json
node "${ISO_DIR}/src/cli.js" < /tmp/iso-one.json 2>&1 | head -46

[ -n "${OUT_DIR}" ] && echo && echo "messages written to ${OUT_DIR}"

echo
echo "I_EXIT=${I_EXIT}"
exit "${I_EXIT}"
