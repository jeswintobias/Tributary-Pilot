# Claude Code prompt — Tributary Pilot (DRUNIX Hackathon, Citi × NPCI)

Paste everything below this line into Claude Code, run from an empty folder that will become the repository.

---

You are helping my team build **Tributary Pilot**, our entry for the DRUNIX Hackathon (Citi × NPCI, India Blockchain Forum, challenge CHL-7007), track: **Cross-Border Remittances**. Read this whole brief before writing any code, then propose a plan for Phase 0 and wait for my go-ahead.

## 1. What we are building

Remittances into India are expensive and capital-heavy because institutions in the chain share no trusted ledger. Tributary Pilot fixes this with four layers on NPCI's **DRUNIX** (an enhanced fork of Hyperledger Fabric v2.5):

1. **Shared corridor liquidity pool with netting.** Sending institutions abroad and Indian payout banks share tokenized INR / foreign-currency deposits. Opposite flows (into and out of India) are netted in short windows; only net positions settle.
2. **AI liquidity agent ("Pilot").** Forecasts corridor demand (a range, not a single number), sizes the pool ahead of demand, and proposes top-ups, auctions or intraday repo. **The AI only proposes; a policy chaincode decides.**
3. **Compliance once, reused everywhere.** KYC/sanctions results are recorded as signed attestations (salted hashes, expiry). Settlement refuses to run without a valid attestation. **No PII on-chain**: PII stays in an encrypted off-chain vault.
4. **Sender-programmed payout.** The sender can bind part of a transfer to a purpose (e.g. college fees to a verified payee), split it across family members, and set a deadline with automatic refund. Modelled on RBI's Special Purpose e₹ concept.

Settlement follows NPCI's cross-chain **two-phase commit**: capture/accept → prepare (lock both legs, anchor proof) → verify proofs → commit (release both together) or abort (refund on timeout). Both legs complete or neither does (PvP / DvD), removing Herstatt risk.

Running example used in docs and demos: Ravi in Dubai sends 2,200 AED; ₹30,000 must go to his sister's college, the rest to his mother in Madurai via UPI. All data is synthetic.

## 2. Non-negotiable engineering rules

- **Honesty first.** Never claim something works unless a test or script proves it. Keep a status table in the README (Done / In progress / Planned) and update it as you go. Mark simulated components clearly.
- **Verify, don't guess.** DRUNIX is at https://github.com/npci/drunix. Read its README and sample network docs before writing setup scripts. If a command or API differs from stock Hyperledger Fabric, follow DRUNIX. If something is unclear, stop and ask me.
- **Chaincode in Go.** Money in **integer minor units** (paise, fils, cents), never floats. Rates stored as integer numerator/denominator.
- **Deterministic chaincode.** No wall-clock time (use `stub.GetTxTimestamp()`), no randomness, no unordered map iteration in outputs, no external calls.
- **Idempotency.** Every state-changing call is keyed by a unique transfer ID / nonce; replays are rejected.
- **No PII on the ledger.** Only salted hashes, statuses and amounts. Use private data collections where orgs need restricted data.
- **Access control in chaincode.** Check the caller's MSP ID / role on every privileged function. Maker-checker (proposer ≠ approver) for limit-exceeding actions.
- **Small, reviewable commits** with clear messages. Tests alongside code.
- Upstream DRUNIX code is **not vendored or modified** in our repo; our `network/` folder only wraps it with scripts.

## 3. Repository layout (create this)

```
tributary-pilot/
├── README.md                 # pitch, status table, real-vs-simulated table, quickstart
├── LICENSE                   # Apache-2.0 (confirm compatible with upstream DRUNIX licence)
├── docs/
│   ├── ARCHITECTURE.md       # layers, orgs, channels, contracts, data flow, diagrams (Mermaid)
│   ├── SETTLEMENT_FLOW.md    # two-phase commit sequence + state machine
│   ├── THREAT_MODEL.md       # threats → controls → tests
│   ├── ASSUMPTIONS.md        # synthetic-data patterns, cost model, capital-savings model
│   └── ROADMAP.md
├── network/                  # scripts wrapping the DRUNIX test network
├── chaincode/
│   ├── settlement/           # Phase 0
│   ├── token/                # Phase 1
│   ├── pool/                 # Phase 1 (pool + netting)
│   ├── policy/               # Phase 1
│   ├── attestation/          # Phase 1
│   └── payout/               # Phase 1
├── services/                 # Phase 1: gateway, fx-oracle, coordinator, payout-adapter, compliance
├── agent/                    # AI liquidity agent (Python)
│   ├── data/                 # synthetic data generator
│   └── notebooks/            # Phase 0 forecasting notebook
├── web/                      # Phase 1: sender app, treasury dashboard, regulator view
├── tests/
│   ├── adversarial/
│   └── benchmark/            # Hyperledger Caliper configs
└── docker-compose.yml
```

## 4. Phase 0 — submit-ready spike (deadline: 25 Oct 2026, ~10 pm IST)

Goal: prove we can run DRUNIX and implement the riskiest piece. Keep scope tight; quality over breadth.

**P0.1 Repository + docs skeleton.** README (pitch, status table, real-vs-simulated table), ARCHITECTURE.md with a Mermaid layer diagram and the four orgs, SETTLEMENT_FLOW.md with the 2PC sequence diagram and state machine, THREAT_MODEL.md, ASSUMPTIONS.md.

**P0.2 DRUNIX test network.** Linux + Docker. Bring up the upstream sample network with at least three orgs representing: `SenderOrg` (UAE money transfer operator), `PayoutBankOrg` (Indian bank), `PlatformOrg` (NPCI platform node / regulator observer). Scripts: `network/up.sh`, `network/down.sh`, `network/deploy-cc.sh <name>`. Document exact prerequisites and versions that worked.

**P0.3 Settlement chaincode (Go).** Single-ledger version of the two-phase commit:
- `CreateTransfer(transferID, senderLegAmount, senderCcy, payoutLegAmount, payoutCcy, quoteID, expiresAt)` → state `CAPTURED`
- `Prepare(transferID)` → locks both legs from simple per-org balances (a minimal in-contract balance map is fine for Phase 0; the full token contract comes in Phase 1) → `PREPARED`, records a proof hash
- `Commit(transferID)` → only if `PREPARED` and not expired → moves both legs atomically → `SETTLED`
- `Abort(transferID)` → callable by either party after expiry, or on failure → releases locks → `ABORTED`
- `GetTransfer`, `GetBalance`, emits chaincode events on each state change
- Endorsement: both `SenderOrg` and `PayoutBankOrg` must endorse `Commit`.

**P0.4 Tests.** Go unit tests with a mock stub covering: happy path; commit without prepare fails; commit after expiry fails; abort refunds both legs; replayed transferID rejected; unauthorized org cannot commit; amounts remain integer-exact. Plus a script that runs the happy path and the abort path on the live network and prints the resulting states.

**P0.5 Forecasting notebook (Python).** `agent/data/generate.py` creates synthetic hourly corridor flows (UAE→IN, UK→IN, US→IN, IN→UAE) with documented assumptions: salary-window spikes (e.g. 25th–5th), festival spikes (Onam, Diwali, Eid), weekly pattern, noise. `agent/notebooks/forecast.ipynb` trains a quantile model (LightGBM) for P50/P90 demand and shows a simple pool-sizing calculation comparing **separate per-provider buffers vs one shared pool** at the same shortfall risk. Report the result as a modeled estimate with assumptions, not a real-world claim.

**P0.6 Final check.** Update README status table to exactly what works. Write a 5-line "Current stage of development" summary I can paste into the submission form.

If P0.2 gets stuck for more than ~3 hours, stop, tell me, and we submit with docs + chaincode unit tests only.

## 5. Phase 1 — coding phase (10–22 Nov 2026, only if shortlisted)

Build in this order, each with tests and README status updates:

1. **Token contract** (MIST-aligned interface: Issue, Manage [burn/split/merge], Settle, Transact). Role-restricted mint/burn with regulator endorsement.
2. **Pool + netting contract.** Deposits/withdrawals per member; netting windows compute multilateral net positions; only net amounts settle.
3. **Policy contract.** Limits (max auto-movement, min pool balance, approved members), maker-checker. Every agent action must pass through it.
4. **Attestation contract.** KYC/sanctions attestations as salted hashes with expiry and issuer; settlement checks validity in the same transaction.
5. **Payout contract.** Purpose-bound vouchers redeemable only by a verified payee ID, splits, deadline with auto-refund.
6. **Cross-ledger coordinator.** Represent the foreign side as a second channel/network; a coordinator service runs NPCI's 2PC (prepare → anchor proof → verify → commit/abort). Label as "NPCI Interop/Coordinator pattern — simulated".
7. **Services (Node.js/TypeScript).** Gateway (Fabric Gateway SDK, roles), FX oracle (signed quotes with expiry and slippage bound), payout adapter (simulated UPI / e₹), event listener, compliance service + encrypted PII vault (Postgres).
8. **AI agent (Python).** Quantile forecaster → optimiser (cost of idle capital vs cost of shortfall, OR-Tools/SciPy) → proposed actions (top-up, auction, intraday repo) submitted via policy contract → audit log. Optional LLM layer only to explain decisions in plain English (model-agnostic; NPCI's AiNxt tooling is an option). The LLM never does the maths or moves money.
9. **Web (Next.js).** Sender app (total-cost quote, purpose rules, tracking), receiver/payee view, treasury dashboard (pool, forecast vs actual, agent decisions + explanations, approvals), regulator view.
10. **Adversarial suite + benchmarks.** Double-spend, settle without attestation, forged/stale FX quote, agent exceeding limits, under-endorsed transaction — all must fail. Hyperledger Caliper for TPS/latency; k6 for API load. Publish measured numbers only.
11. **Deployment notes** for scaling with NPCI Falcon (Kubernetes/Helm).

## 6. Real vs simulated (keep this table in the README)

| Real (runs in our repo) | Simulated |
|---|---|
| DRUNIX network, chaincode, endorsement policies, events, private data | Foreign ledger (second channel), bank core systems |
| Settlement 2PC, netting, policy, attestation and payout logic | UPI / e₹ payout, sanctions list, KYC provider |
| Forecaster, optimiser, dashboards | FX market feed (signed simulated quotes), all customer and flow data |

## 7. How to work with me

- Start by reading the DRUNIX repo and summarising: prerequisites, how the sample network starts, chaincode deployment steps, and anything that differs from stock Fabric.
- Then propose the Phase 0 plan with time estimates and wait for approval.
- Ask before adding dependencies, changing architecture, or anything irreversible.
- After each milestone: what's done, how it was verified, what's next.
