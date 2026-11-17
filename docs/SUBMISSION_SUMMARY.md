# Current stage of development

Five lines for the submission form. Everything in it is backed by something in
this repository that a reviewer can run.

---

Tributary Pilot runs on NPCI's DRUNIX today: a two-phase-commit settlement
chaincode in Go following NPCI's capture → accept → prepare → commit flow,
deployed to a live three-org test network — SenderOrg, PayoutBankOrg and
PlatformOrg as regulator observer — under an
`AND(SenderOrg, PayoutBankOrg)` endorsement policy, where a remittance's
foreign-currency leg and its INR payout leg move atomically in one transaction —
or neither does.

Correctness is demonstrated, not asserted: 187 Go unit tests (76.8% statement
coverage) cover commit-without-prepare, commit-after-expiry, replayed transfer
IDs, double-spend via a second prepare, non-party callers and integer exactness
at 2^53+1; on the live network, a `Commit` endorsed by only one organisation
leaves the transfer `PREPARED`, a sender that tries to `Prepare` before the
payout bank has accepted is refused, and PlatformOrg — a full channel member —
is refused every state-changing call on a transfer it is not a party to while
still being able to read it.

Value is never written as a balance: every movement appends one immutable
double-entry journal line applying a single amount to both a debit and a
credit, so an unbalanced entry cannot be expressed and conservation is a
property of the data model rather than an assertion a test remembers to make —
verified on-chain, every currency summing to exactly zero across all accounts.

Alongside the atomic two-phase commit there is a second, netting-first
settlement path, because 2PC alone does not scale: every transfer on it writes
the payer's balance key, so concurrent transfers collide on MVCC and adding
peers cannot help, the contention being on the key rather than the CPU. An
obligation instead writes only its own unique record and the submitter's
exposure reservation, the netting window is derived arithmetically from the
transaction timestamp so that not even a window pointer is shared, and a window
may only be closed once it is in the past — which is what makes the closing
range query immune to phantom reads, since no transaction can write into a
window its own timestamp does not name. On the live network six obligations
worth 1,800,000 fils gross settled as a single posting of 100,000, a 94.44%
compression, with every account's balance moving by exactly its net position;
and across three institutions a cycle of debts cancelled so completely that the
window settled by writing no balance at all.

A transfer can now only exist at a rate somebody signed: an off-chain oracle
signs a quote and the chaincode verifies the signature, the expiry against the
transaction timestamp, and that the two legs actually sit at that rate within a
stated tolerance — so a forged rate, yesterday's rate, and a genuine rate
attached to unrelated legs are each refused on the live network. It can only
settle to a beneficiary somebody screened: live KYC and sanctions attestations
are required at prepare and **again at commit**, so a sanctions hit arriving
while funds are locked stops the payout rather than arriving too late, which is
demonstrated on-chain. Operating limits sit behind maker-checker, where the
organisation that proposes a change may not be the one that approves it, and
payouts can be bound to their purpose: a remittance splits across named,
screened recipients with a deadline, and whatever is never claimed returns.

Money is held in integer minor units throughout, with no floating-point amount
anywhere in the contract and no personally identifying information on the
ledger.

The AI liquidity agent's forecasting half is built — a synthetic four-corridor
generator and LightGBM quantile models — and on that synthetic data a shared
netted pool needs 37% less buffer than separate per-provider buffers at matched
1% shortfall risk, a modelled estimate under assumptions documented in
`docs/ASSUMPTIONS.md`, not a real-world claim.

Phase 1 adds the token, policy, attestation and payout contracts, pool deposits
and withdrawals, the cross-ledger coordinator, the agent's optimiser behind the
policy contract, the web applications and a Caliper benchmark; the repository's
status table marks precisely what works today and what does not.

---

## Shorter variant, if the form has a tight character limit

> Tributary Pilot settles cross-border remittances atomically on NPCI's DRUNIX.
> A Go two-phase-commit chaincode is deployed to a live test network under an
> `AND(SenderOrg, PayoutBankOrg)` endorsement policy, following NPCI's capture →
> accept → prepare → commit flow: both legs move in one transaction or neither
> does. On the live network a single-org commit is refused, a sender cannot lock
> the payout bank's funds before it accepts, and a third org that is not a party
> is refused while still able to read. 39 unit tests at 77.5% coverage;
> money is integer minor units and no PII touches the ledger. The AI agent's
> quantile forecaster is built, and on synthetic data a shared netted pool needs
> 37% less buffer than separate ones at matched risk — a modelled estimate, not a
> real-world claim. Phase 1 adds the token, pool, policy, attestation and payout
> contracts, the cross-ledger coordinator, the web apps and benchmarks.

## What we would not claim

For whoever fills in the form: these are the overstatements to avoid, because
the repository does not support them.

- Not "three orgs each running peers". PlatformOrg is a channel member with an
  identity and read access and runs no peer of its own; that is all a regulator
  observer needs, and its nodes are Phase 1.
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
- Not "compliance solved". Attestations gate settlement, which is real, but an
  attestation is a named issuer's dated claim that they performed a check — the
  ledger holds them accountable for having said so and cannot know whether they
  looked. Screening is per-beneficiary rather than per-transaction, and a
  salted hash is only as private as its salt.
- Not "the oracle is trustworthy". Anyone holding its private key can sign any
  rate and the chaincode will believe it. The key is generated on first use,
  written 0600 and never committed, and it belongs in an HSM. The set of
  organisations that may register one is compiled into the chaincode, so
  changing it needs a redeploy — it belongs in the channel configuration.
