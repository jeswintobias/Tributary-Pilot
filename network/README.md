# Network

Scripts that wrap the **upstream DRUNIX test network**. Nothing from upstream is
vendored in this repository, and nothing in upstream is modified by these
scripts.

## Where upstream lives

`up.sh` clones DRUNIX to `~/.tributary-pilot/drunix` and pins it to the commit
this repository has been tested against:

| | |
|---|---|
| Repository | https://github.com/npci/drunix |
| Commit | `8090e0ce58802c8a0f13d23a6abe5a2d35456cc5` |
| Commit date | 17 Oct 2026 |
| Licence | Apache-2.0, same as ours |

Override with `DRUNIX_REF` or `DRUNIX_DIR` if you want a different one; the
scripts warn when the checkout does not match the tested commit.

**Why outside the repository, and not under `network/`?** Two reasons. It keeps
upstream unambiguously un-vendored, and DRUNIX's `Makefile` does not quote
`GOBIN`, so a parent directory containing a space — this project lives under
`Drunix AI Agent Hackthon` — makes the build die with
`/bin/sh: AI: command not found`. `~/.tributary-pilot` has no spaces in it.

## Prerequisites

DRUNIX's own README states **Linux**, git, Docker, Golang and jq. What actually
worked here:

| Tool | Version verified | Notes |
|---|---|---|
| macOS | 15.x, Darwin 25.6.0, **arm64** | Not Linux. See the emulation note below |
| Docker Engine | 29.7.2 (server `linux/aarch64`) | Docker Desktop, 8 GB allocated |
| Docker Compose | v5.4.0 | |
| Go | 1.27.3 darwin/arm64 | DRUNIX's `go.mod` asks for 1.26.1; newer is fine |
| jq | 1.7.1 | |
| git | 2.55.0 | |

### The emulation problem, stated plainly

DRUNIX publishes **`linux/amd64` images only**. Checked on Docker Hub for every
image the test network uses:

| Image | Architectures published |
|---|---|
| `npcioss/drunix-peer:1.0.0` | `linux/amd64` |
| `npcioss/drunix-orderer:1.0.0` | `linux/amd64` |
| `npcioss/drunix-vscc:1.0.0` | `linux/amd64` |
| `npcioss/drunix-ccenv:1.0.0` | `linux/amd64` |
| `npcioss/drunix-baseos:1.0.0` | `linux/amd64` |

There is no arm64 build. On Apple silicon every DRUNIX container therefore runs
under emulation. `common.sh` sets `DOCKER_DEFAULT_PLATFORM=linux/amd64`
automatically on `darwin/arm64` — that is the whole workaround, and it means
upstream's compose files do not have to be touched.

It works. `docker run --platform linux/amd64 npcioss/drunix-peer:1.0.0 peer version`
reports `Version: 1.0.0`, `OS/Arch: linux/amd64`. It is **slow**: expect the
network bring-up and each chaincode invoke to take several times what they would
on an amd64 Linux host. For anything resembling a benchmark, use amd64 Linux.

The CLI binaries (`peer`, `cryptogen`, `configtxgen`, …) are built from source by
`ensure_binaries` and run **natively** on the host, so only the network services
are emulated.

## Organisations

The DRUNIX sample network ships two peer organisations and, unlike stock
Hyperledger Fabric, no `addOrg3` sample. Our roles map onto them:

| Our role | Real-world | MSP |
|---|---|---|
| `SenderOrg` | UAE money transfer operator | `Org1MSP` |
| `PayoutBankOrg` | Indian payout bank | `Org2MSP` |
| `PlatformOrg` | NPCI platform node / regulator observer | **not stood up in Phase 0** |

See [`docs/ARCHITECTURE.md` §3](../docs/ARCHITECTURE.md#3-organisations).

The topology upstream brings up is one orderer plus three peers per org, and
YugabyteDB as the state database with KeyDB as the shared transient store — the
DRUNIX architecture rather than stock Fabric's. The three peers per org are not
three replicas of the same thing:

| Container | Role | `setGlobals` index | Host port (org1 / org2) |
|---|---|---|---|
| `lp1.orgN` | **Lite Peer** — endorses, stateless, chaincode installed here | `0` | 7051 / 9051 |
| `cp.orgN` | **Committing Peer** — validates, MVCC, commits | `1` | 7061 / 9061 |
| `vs1.orgN` | **Validation Service** — endorsement-policy and signature checks | `2` | 7071 / 9071 |

This catches people out: upstream's `setGlobals <org>` defaults to index 1, the
Committing Peer, which has no chaincode installed, so every query against it
fails. Our scripts always ask for index 0.

## Usage

```bash
./network/up.sh                      # clone + build (first run), start, create channel
./network/deploy-cc.sh settlement    # package, install, approve, commit
./network/demo.sh                    # happy path + abort path against the live network
./network/down.sh                    # tear down and clean up
```

First run does a shallow clone and a full Go build of eight binaries, so give it
time. Subsequent runs skip both.

### Endorsement policy

`deploy-cc.sh` deploys with:

```
AND('Org1MSP.peer','Org2MSP.peer')
```

so neither organisation can settle a transfer the other has not endorsed. This is
control **T2.2** in [`docs/THREAT_MODEL.md`](../docs/THREAT_MODEL.md), and
`demo.sh` tries a deliberate single-organisation commit to show it being refused.

Note the limitation honestly: Fabric applies a chaincode-level endorsement policy
to **every** function, not only `Commit`. Per-function policy needs state-based
endorsement and is Phase 1 work. Erring toward the stricter policy is the safe
direction.

### Chaincode staging

`deploy-cc.sh` copies the chaincode to `~/.tributary-pilot/cc-stage/<name>` and
runs `go mod vendor` there before deploying. Two reasons: upstream's `deployCC`
does not survive a source path containing a space, and the chaincode builder
container has no network access, so dependencies have to travel with the source.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `/bin/sh: AI: command not found` during the build | A space in the path to the DRUNIX checkout | Use the default `~/.tributary-pilot/drunix`, or any path without spaces |
| `the Docker daemon is not reachable` | Docker Desktop is not running | Start Docker Desktop and retry |
| `no matching manifest for linux/arm64` | `DOCKER_DEFAULT_PLATFORM` not set | `common.sh` sets it on Apple silicon; export it manually elsewhere |
| Bring-up times out | Emulated containers are slow | Raise `-d` (CLI delay) and `-r` (retries); give Docker more memory |
| `chaincode definition for 'X' exists, but chaincode is not installed` on a query that should work | The CLI is pointed at the Committing Peer. DRUNIX splits the peer in two and upstream's `setGlobals <org>` defaults to peer index **1**, the Committing Peer, which has no chaincode installed | Call `setGlobals <org> 0` — index 0 is the Lite Peer, which endorses. `demo.sh` does this via `LITE_PEER=0` |
| `timed out waiting for txid on all peers` | `peer chaincode invoke --waitForEvent` exceeding its 30s default under emulation | Drop `--waitForEvent` and verify by reading committed state back, as `demo.sh` does |
| `OVERRIDE_ORG: unbound variable` | Upstream's `envVar.sh` is not written for `set -u` | `set +u` before sourcing it |
| Local binaries and docker images "out of sync" | Built binaries differ from image 1.0.0 | Rebuild from the pinned commit: `rm -rf ~/.tributary-pilot/drunix && ./network/up.sh` |
