# Tributary Pilot

**Atomic, AI-managed liquidity for cross-border remittance settlement into India, on NPCI's DRUNIX.**

DRUNIX Hackathon · Citi × NPCI · India Blockchain Forum · challenge CHL-7007 · track: Cross-Border Remittances

---

## The problem

A remittance into India crosses a sending institution abroad, one or more
correspondent banks, and an Indian payout bank. None of them share a ledger. So
each one pre-funds its own buffer against flows it cannot see, and nobody can
release their leg until they trust that the other leg happened. That is why the
corridor is capital-heavy and why settlement is slow — two symptoms of one cause.

## What Tributary Pilot does

Four layers on DRUNIX, NPCI's enhanced fork of Hyperledger Fabric v2.5.x:

1. **Shared corridor liquidity pool with netting.** Sending institutions and Indian
   payout banks share tokenised INR and foreign-currency deposits. Opposite flows
   are netted in short windows; only net positions settle.
2. **AI liquidity agent ("Pilot").** Forecasts corridor demand as a *range*, sizes
   the pool ahead of demand, and proposes top-ups, auctions or intraday repo.
   **The agent only proposes. A policy chaincode decides.** The agent holds no key
   that can move money.
3. **Compliance once, reused everywhere.** KYC and sanctions results are recorded
   as signed attestations — salted hashes with an issuer and an expiry. Settlement
   refuses to run without a valid one. **No PII on the ledger**; it stays in an
   encrypted off-chain vault.
4. **Sender-programmed payout.** The sender can bind part of a transfer to a
   purpose, split it across family members, and set a deadline with automatic
   refund. Modelled on RBI's Special Purpose e₹ concept.

Settlement follows NPCI's cross-chain **two-phase commit**: capture → prepare
(lock both legs, anchor proof) → verify → commit, or abort with refund on timeout.
Both legs complete or neither does, which removes Herstatt risk.

**Running example, used throughout the docs and the demo:** Ravi in Dubai sends
2,200 AED. ₹30,000 must reach his sister's college; the rest goes to his mother in
Madurai over UPI. All data is synthetic.

## Status

Updated as work lands. **If it is not marked Done, it does not work yet.**

| # | Component | Status | How it is verified |
|---|---|---|---|
| P0.1 | Docs: architecture, settlement flow, threat model, assumptions, roadmap | **Done** | Read them in [`docs/`](docs/) |
| P0.1 | README with status and real-vs-simulated tables | **Done** | This file |
| P0.3 | Settlement chaincode (Go) — 2PC state machine | **In progress** | — |
| P0.4a | Go unit tests with mock stub | **In progress** | — |
| P0.2 | DRUNIX test network bring-up scripts | **In progress** | — |
| P0.4b | Live happy-path and abort-path scripts | **Planned** | — |
| P0.5 | Synthetic corridor data generator | **Planned** | — |
| P0.5 | Quantile forecasting notebook + pool sizing | **Planned** | — |
| — | `PlatformOrg` as a third organisation | **Planned** | DRUNIX's sample network ships two orgs and no `addOrg3`; see [ARCHITECTURE.md §3](docs/ARCHITECTURE.md#3-organisations) |
| — | Token, pool, policy, attestation, payout contracts | **Planned** | Phase 1 — see [ROADMAP.md](docs/ROADMAP.md) |
| — | Services, web apps, AI agent, benchmarks | **Planned** | Phase 1 |

No throughput, latency or cost figure appears anywhere in this repository. None
will until it is measured.

## Real vs simulated

| Real — runs in this repo | Simulated |
|---|---|
| DRUNIX network, chaincode, endorsement policies, events, private data | Foreign ledger (second channel), bank core systems |
| Settlement 2PC, and in Phase 1 netting, policy, attestation and payout logic | UPI / e₹ payout, sanctions list, KYC provider |
| Forecaster, optimiser, dashboards | FX market feed (signed simulated quotes), **all customer and flow data** |

## Quickstart

### Prerequisites

| Tool | Version used here | Notes |
|---|---|---|
| Go | 1.27.3 (darwin/arm64) | Needed for the chaincode and its unit tests |
| Docker + Compose | 29.7.2 / v5.4.0 | Needed only for the live network |
| Python | 3.12.0 | Needed only for the agent and notebook |
| jq, git | 1.7.1, 2.55.0 | Required by DRUNIX's `network.sh` |

DRUNIX documents **Linux** as its prerequisite. This repository is being developed
on macOS arm64; see [`docs/ASSUMPTIONS.md` §6](docs/ASSUMPTIONS.md#6-environment)
and `network/README.md` for what that means in practice.

### Chaincode tests — no Docker, no network needed

```bash
cd chaincode/settlement
go test ./... -v
```

### Live network

See [`network/README.md`](network/README.md). Upstream DRUNIX is **not vendored**
in this repository; the scripts clone it alongside and wrap its `network.sh`.

```bash
./network/up.sh                      # bring up the network and create the channel
./network/deploy-cc.sh settlement    # package, install, approve and commit
./network/down.sh                    # tear down and clean
```

## Repository layout

```
tributary-pilot/
├── docs/          architecture, settlement flow, threat model, assumptions, roadmap
├── network/       scripts wrapping the DRUNIX test network (upstream not vendored)
├── chaincode/     settlement (Phase 0) · token, pool, policy, attestation, payout (Phase 1)
├── services/      Phase 1 — gateway, fx-oracle, coordinator, payout-adapter, compliance
├── agent/         AI liquidity agent — data/ generator, notebooks/ forecasting
├── web/           Phase 1 — sender app, treasury dashboard, regulator view
└── tests/         adversarial/ and benchmark/ (Caliper)
```

## Engineering rules we hold ourselves to

- **Honesty first.** Nothing is claimed to work unless a test or a script proves
  it. The status table above is the single source of truth.
- Money in **integer minor units** — paise, fils, cents. No floats, anywhere.
  Rates as integer numerator and denominator.
- **Deterministic chaincode.** `stub.GetTxTimestamp()` not wall-clock time, no
  randomness, no map iteration in outputs, no external calls.
- **Idempotency.** Every state-changing call is keyed by a unique transfer ID.
  Replays are rejected.
- **No PII on the ledger.** Salted hashes, statuses and amounts only.
- **Access control in chaincode**, checked on every privileged function, with
  maker-checker for limit-exceeding actions.
- Upstream DRUNIX is never vendored or modified here.

## Licence

Apache-2.0, matching upstream DRUNIX. See [LICENSE](LICENSE).
