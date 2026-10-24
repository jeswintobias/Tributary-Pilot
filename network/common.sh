#!/usr/bin/env bash
# Shared setup for the Tributary Pilot network scripts.
#
# These scripts wrap the upstream DRUNIX test network. Upstream is cloned
# alongside this repository and is never vendored or modified — see
# network/README.md.

set -euo pipefail

NETWORK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${NETWORK_DIR}/.." && pwd)"

# The upstream commit this repository has actually been tested against. Override
# DRUNIX_REF to try a different one.
DRUNIX_REPO="${DRUNIX_REPO:-https://github.com/npci/drunix.git}"
DRUNIX_REF="${DRUNIX_REF:-8090e0ce58802c8a0f13d23a6abe5a2d35456cc5}"
# Upstream lives OUTSIDE the repository, at a path with no spaces in it.
# DRUNIX's Makefile does not quote GOBIN, so a parent directory containing a
# space ("Drunix AI Agent Hackthon") makes the build fail with
# "/bin/sh: AI: command not found". Keeping upstream out of the project tree
# sidesteps that and reinforces that we never vendor it.
TRIBUTARY_HOME="${TRIBUTARY_HOME:-${HOME}/.tributary-pilot}"
DRUNIX_DIR="${DRUNIX_DIR:-${TRIBUTARY_HOME}/drunix}"
TEST_NETWORK_DIR="${DRUNIX_DIR}/drunix-network/test-network"

# Channel and org naming. Org1/Org2 are the stock test-network organisations;
# we map our roles onto them. See docs/ARCHITECTURE.md section 3 for why
# PlatformOrg is not stood up in Phase 0.
CHANNEL_NAME="${CHANNEL_NAME:-mychannel}"
SENDER_ORG_MSP="Org1MSP"       # SenderOrg — UAE money transfer operator
PAYOUT_ORG_MSP="Org2MSP"       # PayoutBankOrg — Indian payout bank

log()  { printf '\033[1;34m[tributary]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[tributary]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[tributary]\033[0m %s\n' "$*" >&2; exit 1; }

# DRUNIX publishes linux/amd64 images only. On Apple silicon every container
# therefore runs under emulation, which works but is slow. Setting this here
# keeps us from having to edit upstream's compose files, which we do not touch.
configure_platform() {
  if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ]; then
    export DOCKER_DEFAULT_PLATFORM="${DOCKER_DEFAULT_PLATFORM:-linux/amd64}"
    warn "Apple silicon detected: forcing DOCKER_DEFAULT_PLATFORM=${DOCKER_DEFAULT_PLATFORM}."
    warn "DRUNIX ships amd64 images only, so containers run emulated and are slow."
  fi
}

# Upstream's scripts break on paths containing spaces. Our own repo path may
# contain them, so anything we hand to upstream is staged under TRIBUTARY_HOME.
assert_no_spaces() {
  case "$1" in
    *" "*) die "$2 contains a space: $1" ;;
  esac
}

require_tools() {
  local missing=()
  for tool in docker git jq go; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done
  [ ${#missing[@]} -eq 0 ] || die "missing required tools: ${missing[*]}"
  docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable — start Docker and retry"
}

# Clone upstream DRUNIX next to this repo if it is not already there, and pin it
# to the commit we tested. Nothing from upstream is copied into this repository.
ensure_upstream() {
  if [ ! -d "${DRUNIX_DIR}/.git" ]; then
    log "cloning upstream DRUNIX into ${DRUNIX_DIR} (outside the repo, never vendored)"
    mkdir -p "$(dirname "${DRUNIX_DIR}")"
    git clone "${DRUNIX_REPO}" "${DRUNIX_DIR}"
    git -C "${DRUNIX_DIR}" checkout --quiet "${DRUNIX_REF}"
  fi
  [ -f "${TEST_NETWORK_DIR}/network.sh" ] || die "upstream test network not found at ${TEST_NETWORK_DIR}"

  local head
  head="$(git -C "${DRUNIX_DIR}" rev-parse HEAD)"
  if [ "${head}" != "${DRUNIX_REF}" ]; then
    warn "upstream is at ${head}, not the tested ${DRUNIX_REF}"
  fi
}

# The peer builds chaincode by shelling out to `docker build` against these base
# images. They are not part of the compose topology, so nothing pulls them for
# you, and a missing one fails chaincode install with a bare
# "No such image: npcioss/drunix-ccenv:1.0".
ensure_cc_builder_images() {
  for image in npcioss/drunix-ccenv:1.0 npcioss/drunix-baseos:1.0; do
    if ! docker image inspect "${image}" >/dev/null 2>&1; then
      log "pulling chaincode builder image ${image}"
      docker pull ${DOCKER_DEFAULT_PLATFORM:+--platform "${DOCKER_DEFAULT_PLATFORM}"} "${image}"
    fi
  done
}

# Build the DRUNIX CLI binaries. They run natively on the host; only the peers,
# orderer and validation service run in (emulated) containers.
ensure_binaries() {
  if [ -x "${DRUNIX_DIR}/build/bin/peer" ] && [ -x "${DRUNIX_DIR}/build/bin/cryptogen" ]; then
    return
  fi
  log "building DRUNIX binaries (first run only, this takes several minutes)"
  ( cd "${DRUNIX_DIR}" && make peer orderer configtxgen configtxlator osnadmin cryptogen discover ledgerutil )
}

# Put the freshly built binaries and upstream's config on the path, exactly as
# upstream's own README instructs.
export_fabric_env() {
  export PATH="${DRUNIX_DIR}/build/bin:${PATH}"
  # Upstream's README says "../config" relative to test-network, which is
  # drunix-network/config — not a top-level config/ directory, which DRUNIX does
  # not have.
  export FABRIC_CFG_PATH="${DRUNIX_DIR}/drunix-network/config"
}
