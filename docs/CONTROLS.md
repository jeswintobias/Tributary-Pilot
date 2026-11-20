# What the ledger refuses

Phases 0 to C made settlement work: atomic, conserved, netted, projected and
paid. Everything in them is about the system doing something.

This is about the system declining to. Four controls, each closing a hole that
was real and open until it landed:

| Control | The hole it closes |
|---|---|
| Signed FX quotes | the sender chose both legs and named a rate nobody checked |
| KYC and sanctions attestations | the ledger settled to anyone, screened or not |
| Policy with maker-checker | no limit existed, and nothing stopped one key changing that |
| Purpose-bound vouchers | where the money went after arrival was an off-chain promise |

---

## 1. Signed FX quotes

### What it was

`CreateTransfer` took a `quoteID` that was a string and nothing more. It was
stored, folded into the proof hash, and never checked against anything. The
sending organisation chose both legs and named a rate: **2,200 AED for 1 INR
was as acceptable to the contract as the real rate**, and so was a rate that
had been true yesterday.

The counterparty's protection was that it had to `Accept`. That is a real
control and it is not enough — it makes the payout bank re-derive the rate
off-chain for every transfer and trust its own copy of the market, which is the
reconciliation this project exists to remove.

### What a quote is now

An oracle signs a quote off-chain; the chaincode verifies three things, and
none substitutes for another:

1. **The signature.** Anyone can produce a well-formed signature; only the
   registered oracle can produce one that verifies.
2. **The expiry**, against the transaction timestamp. A valid signature on a
   stale quote is yesterday's rate.
3. **That the legs match the rate.** This is what stops a signed quote being
   decoration — a genuine, current quote attached to legs bearing no relation
   to it.

```
expected = senderAmount × rateNum ÷ rateDen
|payoutAmount − expected| ≤ expected × maxSlippageBps ÷ 10000
```

### Rates are rational, not decimal

A rate is an integer numerator over an integer denominator. 1 AED = 22.7431 INR
is `227431/10000`. There is no decimal rate anywhere, for the same reason there
is no floating-point amount: two endorsing peers must compute bit-identical
results, and 22.7431 is not a number a float represents exactly.

The multiply-then-divide runs through `mulDiv`, which keeps the intermediate in
128 bits. `220000 × 227431` passes 2^64 comfortably, and an overflowed
intermediate would produce a rate check that silently passes anything.

### Verification is deterministic; signing is not

ECDSA draws a nonce, so signing is not deterministic — but signing happens off
chain. Verification is a pure function of (public key, message, signature) and
returns the same answer on every peer, which is all the determinism rule
requires.

### The encoding exists twice, and that is pinned down

`quote.go` implements the canonical bytes in Go; `services/oracle/src/quote.js`
implements them in JavaScript. They must agree byte for byte or every quote the
service signs is rejected on chain with "not signed by oracle" and no clue as
to why.

Nothing but care keeps two implementations of a wire format in step, which is
not enough for something that fails this confusingly. So `npm run vector`
writes a signed quote and its canonical bytes into the chaincode's testdata,
and `TestQuote_MatchesTheOracleServiceEncoding` re-derives those bytes with the
Go implementation and verifies that signature with the Go verifier. Dropping
the length prefix on the JavaScript side, or changing the domain separator,
each fail it.

Fields are length-prefixed as `<byte-length>:<field>`. Without that, adjacent
fields run together and two different quotes serialise identically — a
signature over one would verify the other. Byte length, not character count,
because Go measures `len(string)` in bytes.

### A quote is dealt once

Single use, keyed by quote ID. Reuse would let a sender take one favourable
quote and keep spending it while the market moved away from it, which is the
exposure the expiry is meant to bound.

### What this rests on

**One key.** Anyone holding the oracle's private key can sign any rate and the
chaincode will believe it. It lives in one process, is generated on first use,
is written `0600`, and is never committed. A real rate oracle's key belongs in
an HSM. The set of organisations that may register a key is a constant in the
chaincode, which means adding or removing an oracle needs a redeploy —
production wants it in the channel configuration.

---

## 2. KYC and sanctions attestations

### What it was

The only thing the contract knew about a beneficiary was an opaque reference.
Whether that person had been identified, or screened against a sanctions list,
was somebody else's business happening somewhere else, and the ledger settled
regardless. The submission summary said so in as many words: *not "compliance
built in"*.

### The gate

Two attestations — `KYC` and `SANCTIONS` — must be live for a beneficiary
before the payout leg may move. Checked at **Prepare and again at Commit**, and
both, not either:

- at Prepare, so a counterparty's funds are never locked against a beneficiary
  who cannot be paid
- at Commit, **because that is when value actually moves**

Checking only at Prepare would leave a window as long as the settlement window
in which a newly sanctioned beneficiary still gets paid.
`TestCommit_RefusedWhenSanctionsLandMidSettlement` is that case, and the live
run does it on the network: prepared, funds locked, attestation revoked, commit
refused, beneficiary paid nothing.

A sanctions hit is expressed by **revoking**, not by issuing a negative. The
record is kept with the reason, because the fact that somebody was once
attested and then was not is exactly what an auditor needs to see.

### Refunds are not gated

An aborted transfer refunds regardless of compliance. Value returning to the
person it came from is not a payout, and blocking it would let a revocation
strand the counterparty's money for ever.

### Still no PII

An attestation is about a salted hash, never a person. The ledger records that
*some* beneficiary was screened by *some* issuer until *some* date — what an
auditor needs — and nothing that identifies anybody.

**The honest limit:** a salted hash is only as private as its salt. If the salt
leaks, the hash of a known name can be recomputed and matched, and the ledger
then reveals which beneficiaries transacted. The salt lives in the compliance
service and never on the chain. This is a real exposure and hashing alone does
not solve it.

### What an attestation is not

It is not evidence of compliance. It is a claim by a named issuer, with a date
on it, that they performed a check. The ledger holds them accountable for
having said so; **it cannot know whether they looked.**

Validity is capped at a year. A check performed once and trusted for ever is
not a check.

---

## 3. Policy, and maker-checker

### What it is for

Everything else answers "is this transfer valid". A policy answers a different
question: *is this transfer within what we agreed to do at all*. A transfer can
be well formed, correctly priced and settled to a screened beneficiary, and
still be one nobody intended — ten times the usual size, or to an organisation
that was never meant to be a counterparty.

It matters most for the Phase 1 agent. An automated process proposing movements
needs a bound it cannot argue with, and that bound has to live where the agent
cannot reach it.

### Two signatures, and they must differ

Changing a limit is more dangerous than any single transfer, because it changes
every transfer after it. So a change takes two organisations: one proposes, a
**different** one approves. One compromised key raises no limits.

The proposal is keyed by the hash of its own contents, so the approver approves
exactly what was proposed. Approving by ID while the contents could change
underneath would be worse than no second signature at all — it would record an
approval nobody gave.

The hash sorts the limit maps before hashing them. Go randomises map iteration,
so hashing in iteration order would give each endorsing peer a different
proposal ID and nothing would ever be approvable.

### An unset policy imposes nothing

A zero value means "no limit", so a channel with no policy behaves exactly as
it did before one existed. A policy mechanism that bricks the system when unset
is one nobody dares deploy.

### Where it bites

At `CreateTransfer` and `SubmitObligation`, not at settlement. A transfer
outside policy should never exist to be accepted — refusing it later would mean
a counterparty had already agreed to something the channel does not permit.

---

## 4. Purpose-bound vouchers

### The story

Ravi sends money home and wants part of it to reach his sister's college and
the rest to his mother. Today the ledger settles the payout leg to one
beneficiary and the story ends there: what happens next is a promise made
off-chain, and the whole point of settling on a shared ledger was to stop
relying on those.

A voucher binds the money to its purpose. **Value leaves the beneficiary's
balance at issue** and can only reach the recipients the voucher names, for the
purposes it names, before the date it names. What is not redeemed goes back.

### Why not just several transfers

Two reasons. The sender's instruction is one instruction and has to succeed or
fail as one — paying the college but not the mother is not a partial success.
And the deadline belongs to the instruction rather than to each payment: the
refund has to be of whatever is left over, which needs the pieces to share an
identity.

### The rules

- Only the **payout org** may issue, because it holds the beneficiary's funds.
- The splits must account for the **whole** payout leg exactly. A voucher that
  bound only part of it would leave a remainder unaccounted for in a record
  that claims to say where the money went.
- **Every recipient is screened**, exactly as the original beneficiary was. A
  voucher must not be a way to route value to somebody who could not have
  received it directly.
- Screened **again at redemption**, not only at issue: a voucher can outlive a
  sanctions decision, and the money moves at redemption.
- Splits redeem individually, because they are presented individually. The
  college claims when term starts; the family when they need it. Requiring them
  together would make the slowest recipient hold up everyone else.
- The refund goes to the **beneficiary**, not back across the border. The money
  arrived and is theirs; what the voucher constrained was the purpose, and an
  expired constraint should release the funds rather than claw them back.

### What is still off-chain

That the recipient really is the college. The voucher names a salted hash and
an attestation says somebody screened it. Nothing here can tell whether the
institution behind that hash is the one Ravi meant, and no amount of ledger
design changes that.

---

## 5. Running it

```bash
cd services/oracle && npm install && npm start   # the signing service
./network/setup-fx-compliance.sh                 # register the oracle, attest the demo payees
./network/demo-compliance.sh                     # the refusals
./network/demo-voucher.sh                        # Ravi's story end to end
```

`setup-fx-compliance.sh` also proves the two negative controls that the rest
depends on: an organisation that is not an oracle MSP cannot register a signing
key, and an organisation that is not an attestor cannot vouch for its own
beneficiaries.

## 6. What is deliberately not solved

- **Allow-lists are compile-time constants.** `OracleMSPs`, `AttestorMSPs` and
  `PolicyAdminMSPs` live in the chaincode, so changing any of them needs a
  redeploy. They belong in the channel configuration, where a change takes the
  ordinary config-update signatures. This is the weakest part of the design and
  it is stated rather than discovered.
- **One oracle key, on disk.** See above.
- **`maxSlippageBps` is the oracle's statement**, so a client cannot widen it —
  but nothing stops an oracle quoting a tolerance so wide it means nothing. A
  policy cap on slippage is obvious work and is not done.
- **Attestations are per-beneficiary, not per-transfer.** A beneficiary cleared
  once is cleared for every transfer until the attestation expires or is
  revoked. Per-transaction screening is a different and much heavier model.
- **Nothing re-screens on a schedule.** Expiry is the only forcing function.
- **A voucher's purposes are free text.** `TUITION` is recorded, not enforced:
  the chaincode cannot tell whether the recipient is really a college.
- **Policy has no daily or cumulative limits**, only per-transfer caps. A
  velocity limit needs a windowed counter, which is a shared key and would need
  the treatment the netting path got.

---

## 7. Found by deploying, not by testing

Two defects reached the live network after passing every unit test, and both
are worth recording because the same blind spot produces both.

**The unit tests call the contract's Go methods directly.** The peer does not.
It goes through the contract API, which derives a JSON schema from each
parameter type and validates arguments against it before the method is
reached. Everything that layer does is invisible to a test that skips it.

`ProposePolicy` originally took the stored `Policy` type, whose fields include
`version`, `activatedAt`, `proposedBy` and `approvedBy` — set when a change is
activated, not when it is proposed. The generated schema marked them required,
so the live network answered:

```
value did not match schema:
1. param0: activatedAt is required
2. param0: approvedBy is required
3. param0: proposedBy is required
4. param0: version is required
```

The fix is a type split: `PolicyTerms` is what a proposer supplies, `Policy` is
that embedded with the record of who put it in force. The hash covers the terms
only, which it did before — the split just makes that explicit instead of
implicit.

`TestInputTypes_CarryNoFieldsOnlyTheContractFillsIn` pins it. Adding an
activation field back to `PolicyTerms` compiles, and fails that test.

Recorded honestly: `[]VoucherSplit` is also an input and *does* carry fields
filled in later — `redeemed`, `redeemedAt`, `redeemedBy` — and it was accepted
on the live network. Why a slice of structs behaves differently from a struct
has not been established, so nothing asserts a rule about it. The Policy case
is the one that is known.

**The second was not ours at all** — DRUNIX silently truncating range queries
at ten results, which made a conservation check report a false imbalance. That
one has its own write-up in
[DRUNIX_RANGE_QUERY_BUG.md](DRUNIX_RANGE_QUERY_BUG.md).

The lesson both share: a test that is kinder than the platform is how a real
defect ships. The mock now reproduces the truncation deliberately, and the
metadata guard covers the schema layer the mock cannot.
