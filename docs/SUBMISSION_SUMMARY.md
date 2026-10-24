# Current stage of development

Five lines for the submission form. Everything in it is backed by something in
this repository that a reviewer can run.

---

Tributary Pilot runs on NPCI's DRUNIX today: a two-phase-commit settlement
chaincode in Go, deployed to a live two-org test network under an
`AND(SenderOrg, PayoutBankOrg)` endorsement policy, where a remittance's
foreign-currency leg and its INR payout leg move atomically in one transaction —
or neither does.

Correctness is demonstrated, not asserted: 26 Go unit tests (76% statement
coverage) cover commit-without-prepare, commit-after-expiry, replayed transfer
IDs, double-spend via a second prepare, non-party callers and integer exactness
at 2^53+1; on the live network, a `Commit` endorsed by only one organisation
leaves the transfer `PREPARED`, which is the control the whole design rests on.

Money is held in integer minor units throughout, with no floating-point amount
anywhere in the contract and no personally identifying information on the
ledger.

The AI liquidity agent's forecasting half is built — a synthetic four-corridor
generator and LightGBM quantile models — and on that synthetic data a shared
netted pool needs 37% less buffer than separate per-provider buffers at matched
1% shortfall risk, a modelled estimate under assumptions documented in
`docs/ASSUMPTIONS.md`, not a real-world claim.

Phase 1 adds the token, pool, policy, attestation and payout contracts, the
cross-ledger coordinator, the agent's optimiser behind the policy contract, the
web applications and a Caliper benchmark; the repository's status table marks
precisely what works today and what does not.

---

## Shorter variant, if the form has a tight character limit

> Tributary Pilot settles cross-border remittances atomically on NPCI's DRUNIX.
> A Go two-phase-commit chaincode is deployed to a live test network under an
> `AND(SenderOrg, PayoutBankOrg)` endorsement policy: both legs of a transfer
> move in one transaction or neither does, and a single-org commit is refused on
> the live network. 26 unit tests at 76% coverage cover the rejection paths;
> money is integer minor units and no PII touches the ledger. The AI agent's
> quantile forecaster is built, and on synthetic data a shared netted pool needs
> 37% less buffer than separate ones at matched risk — a modelled estimate, not a
> real-world claim. Phase 1 adds the token, pool, policy, attestation and payout
> contracts, the cross-ledger coordinator, the web apps and benchmarks.

## What we would not claim

For whoever fills in the form: these are the overstatements to avoid, because
the repository does not support them.

- Not "running on a three-org network". DRUNIX's sample network ships two orgs
  and no `addOrg3`; `PlatformOrg` is designed and documented, not deployed.
- Not "cross-chain" or "interoperable" yet. Phase 0 is one ledger. The proof
  hash is anchored and re-checked locally, not exchanged with a counterparty
  network.
- Not any TPS or latency number. Nothing has been benchmarked, and it cannot be
  benchmarked meaningfully on the machine it was built on — DRUNIX publishes
  amd64 images only, so everything here ran emulated on Apple silicon.
- Not a rupee or dollar capital saving. The 37% is a buffer ratio on synthetic
  data at a stated quantile and netting window; it ranges 14%–44% across that
  grid, and converting it to money needs a balance sheet and a cost of capital
  we do not have.
- Not "compliance built in". The attestation contract is Phase 1. What exists
  today is the rule that no PII reaches the ledger, and a settlement contract
  with nowhere to put any.
