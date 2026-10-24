#!/usr/bin/env bash
# Add PlatformOrg (Org3MSP) to the running channel as a member.
#
#   ./network/up.sh && ./network/deploy-cc.sh settlement && ./network/add-platform-org.sh
#
# PlatformOrg is the NPCI platform node and regulator observer. It joins as a
# channel member with an admin identity and runs no endorsing peer — see
# platformorg/crypto-config-org3.yaml for why.
#
# This is the standard Fabric add-organisation flow, driven from our repository:
# generate crypto, print the org definition, fetch the channel config, splice
# the org in, compute the delta, collect signatures from the organisations
# already on the channel, submit. Upstream's configtx.yaml is never modified;
# our own minimal one defines only Org3.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

ORG3_MSP="Org3MSP"
ORG3_DOMAIN="org3.example.com"
ORG3_HOME="${TRIBUTARY_HOME}/platformorg"
ORG3_MSPDIR="${ORG3_HOME}/organizations/peerOrganizations/${ORG3_DOMAIN}/msp"

configure_platform
require_tools
ensure_upstream
ensure_binaries
export_fabric_env

assert_no_spaces "${ORG3_HOME}" "the PlatformOrg staging path"

cd "${TEST_NETWORK_DIR}"
# Upstream's helpers are not written for `set -u`.
set +u
# shellcheck disable=SC1091
source ./scripts/configUpdate.sh

LITE_PEER=0
ARTIFACTS="${TEST_NETWORK_DIR}/channel-artifacts"
mkdir -p "${ARTIFACTS}"

step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }

# ---------------------------------------------------------------------------
step "Generating PlatformOrg identities"
# ---------------------------------------------------------------------------
if [ -d "${ORG3_MSPDIR}" ]; then
  log "PlatformOrg crypto material already present at ${ORG3_HOME}"
else
  rm -rf "${ORG3_HOME}"
  mkdir -p "${ORG3_HOME}"
  cryptogen generate \
    --config="${NETWORK_DIR}/platformorg/crypto-config-org3.yaml" \
    --output="${ORG3_HOME}/organizations"
  log "generated into ${ORG3_HOME}/organizations"
fi
[ -d "${ORG3_MSPDIR}" ] || die "expected an MSP at ${ORG3_MSPDIR}"

# ---------------------------------------------------------------------------
step "Printing the PlatformOrg organisation definition"
# ---------------------------------------------------------------------------
# configtxgen reads configtx.yaml from a directory, so the template is
# materialised into the staging area with an absolute MSPDir.
sed "s|__MSPDIR__|${ORG3_MSPDIR}|" \
  "${NETWORK_DIR}/platformorg/configtx-org3.template.yaml" \
  > "${ORG3_HOME}/configtx.yaml"

configtxgen -printOrg "${ORG3_MSP}" -configPath "${ORG3_HOME}" \
  > "${ORG3_HOME}/org3.json"
[ -s "${ORG3_HOME}/org3.json" ] || die "configtxgen produced an empty org definition"
log "organisation definition written to ${ORG3_HOME}/org3.json"

# ---------------------------------------------------------------------------
step "Fetching the current channel configuration"
# ---------------------------------------------------------------------------
fetchChannelConfig 1 "${LITE_PEER}" "${CHANNEL_NAME}" "${ARTIFACTS}/config.json"

if jq -e ".channel_group.groups.Application.groups.${ORG3_MSP}" \
     "${ARTIFACTS}/config.json" >/dev/null 2>&1; then
  log "${ORG3_MSP} is already a member of ${CHANNEL_NAME}; nothing to do"
  exit 0
fi

# ---------------------------------------------------------------------------
step "Splicing PlatformOrg into the application group"
# ---------------------------------------------------------------------------
jq -s ".[0] * {\"channel_group\":{\"groups\":{\"Application\":{\"groups\":{\"${ORG3_MSP}\":.[1]}}}}}" \
  "${ARTIFACTS}/config.json" "${ORG3_HOME}/org3.json" \
  > "${ARTIFACTS}/modified_config.json"

createConfigUpdate "${CHANNEL_NAME}" \
  "${ARTIFACTS}/config.json" \
  "${ARTIFACTS}/modified_config.json" \
  "${ARTIFACTS}/org3_update_in_envelope.pb"

# ---------------------------------------------------------------------------
step "Collecting signatures from the organisations already on the channel"
# ---------------------------------------------------------------------------
# Adding a member needs the existing members' admins to agree. This is the
# maker-checker principle at the channel level: one organisation cannot admit
# another on its own.
# Upstream signature is signConfigtxAsPeerOrg <org> <file> — no peer index.
# Signing is local, so it needs the org admin's MSP and nothing else.
signConfigtxAsPeerOrg 1 "${ARTIFACTS}/org3_update_in_envelope.pb"
log "signed by SenderOrg (Org1MSP)"

setGlobals 2 "${LITE_PEER}"
log "submitting as PayoutBankOrg (Org2MSP), whose submission counts as its signature"
peer channel update \
  -f "${ARTIFACTS}/org3_update_in_envelope.pb" \
  -c "${CHANNEL_NAME}" \
  -o localhost:7050 --ordererTLSHostnameOverride orderer.example.com \
  --tls --cafile "${ORDERER_CA:-${TEST_NETWORK_DIR}/organizations/ordererOrganizations/example.com/tlsca/tlsca.example.com-cert.pem}"

# ---------------------------------------------------------------------------
step "Verifying membership"
# ---------------------------------------------------------------------------
sleep 5
fetchChannelConfig 1 "${LITE_PEER}" "${CHANNEL_NAME}" "${ARTIFACTS}/config_after.json"
if jq -e ".channel_group.groups.Application.groups.${ORG3_MSP}" \
     "${ARTIFACTS}/config_after.json" >/dev/null 2>&1; then
  printf '\033[1;32mOK: %s is now a member of %s\033[0m\n' "${ORG3_MSP}" "${CHANNEL_NAME}"
else
  die "${ORG3_MSP} is not in the channel config after the update"
fi

cat <<EOF

PlatformOrg is on the channel. It has an identity and read access, and runs no
peer. Prove that it still cannot settle someone else's transfer:

  ${REPO_DIR}/network/verify-platformorg.sh
EOF
