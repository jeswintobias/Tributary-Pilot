# Roadmap

## Phase 0 — submit-ready spike (due 25 Oct 2026, ~22:00 IST)

Goal: prove we can run DRUNIX and implement the riskiest piece — atomic
settlement. Scope is deliberately narrow. Live status is in the
[README status table](../README.md#status), which is the single source of truth;
this file is the plan, not the report.

| ID | Deliverable |
|---|---|
| P0.1 | Repository and documentation skeleton |
| P0.2 | DRUNIX test network with scripted bring-up, three orgs intended |
| P0.3 | Settlement chaincode in Go — single-ledger two-phase commit |
| P0.4 | Go unit tests with a mock stub, plus live happy-path and abort-path scripts |
| P0.5 | Synthetic corridor data generator and a quantile forecasting notebook |
| P0.6 | Truth pass on the status table and a 5-line submission summary |

**Stop rule, agreed in advance:** if P0.2 is not working after roughly three hours,
we stop, say so, and submit with documentation and chaincode unit tests only. We do
not ship a claim that the network ran if it did not.

## Phase 1 — coding phase (10–22 Nov 2026, only if shortlisted)

Built in this order, each step with tests and a README status update before the
next one starts.

| # | Deliverable | Depends on |
|---|---|---|
| 1 | **Token contract.** MIST-aligned Issue / Manage (burn, split, merge) / Settle / Transact. Role-restricted mint and burn with regulator endorsement | P0.3 |
| 2 | **Pool + netting contract.** Per-member deposits and withdrawals; netting windows computing multilateral net positions; only net amounts settle | 1 |
| 3 | **Policy contract.** Max auto-movement, minimum pool balance, approved members, maker-checker. Every agent action routes through it | 2 |
| 4 | **Attestation contract.** KYC and sanctions attestations as salted hashes with issuer and expiry; settlement validates in the same transaction | 1 |
| 5 | **Payout contract.** Purpose-bound vouchers redeemable only by a verified payee ID, splits across recipients, deadline with automatic refund | 4 |
| 6 | **Cross-ledger coordinator.** Foreign side as a second channel; a coordinator service runs prepare → anchor proof → verify → commit/abort. Labelled "NPCI Interop/Coordinator pattern — simulated" | 1 |
| 7 | **Services (Node.js / TypeScript).** Gateway on the Fabric Gateway SDK with roles, FX oracle issuing signed quotes with expiry and slippage bound, simulated UPI / e₹ payout adapter, event listener, compliance service with an encrypted Postgres PII vault | 1–5 |
| 8 | **AI agent (Python).** Quantile forecaster → optimiser trading idle-capital cost against shortfall cost → proposed actions submitted through the policy contract → on-ledger audit log. Optional LLM layer explains decisions in plain English and does nothing else | 3, P0.5 |
| 9 | **Web (Next.js).** Sender app with total-cost quote and purpose rules, receiver view, treasury dashboard showing pool state and forecast versus actual with agent decisions and approvals, regulator view | 7 |
| 10 | **Adversarial suite and benchmarks.** Double-spend, settle without attestation, forged and stale FX quote, agent exceeding limits, under-endorsed transaction — each must fail. Caliper for TPS and latency, k6 for API load. Measured numbers only | all |
| 11 | **Deployment notes** for scaling with NPCI Falcon (Kubernetes / Helm) | 10 |

## Deferred beyond Phase 1

- Standing up `PlatformOrg` as a third organisation with a regulator observer
  channel, which the DRUNIX sample network does not provide out of the box
- Multi-corridor netting across more than the four modelled corridors
- Production key management and HSM integration
- Any real payment-rail integration, which needs a licensed counterparty
