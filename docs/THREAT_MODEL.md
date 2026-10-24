# Threat model

> Status: Phase 0. The **Test** column names a real test where one exists today and
> says **Planned** where it does not. A control with a Planned test is a design
> commitment, not a demonstrated property.

## Scope and trust assumptions

In scope: the settlement chaincode, the ledger state it owns, the endorsement
policy that protects it, and the agent's route to moving money. Out of scope for
Phase 0: the foreign ledger, bank core systems, the UPI / e₹ rails, the KYC
provider and the sanctions list — all simulated, all listed in the README's
real-vs-simulated table.

We assume the orderer set is honest-but-curious and that at least the DRUNIX
network's MSP material is correctly issued. We do **not** assume any participating
organisation is honest: `SenderOrg` and `PayoutBankOrg` are each treated as capable
of trying to cheat the other, which is the entire reason for two-phase commit.

## T1 — Settlement and ledger integrity

| # | Threat | Control | Test |
|---|---|---|---|
| T1.1 | One leg moves and the other does not, leaving a party exposed | Both legs move inside a single chaincode transaction; a failed guard reverts the whole write set | `TestCommit_HappyPath_BothLegsMove` |
| T1.2 | A party commits without locking funds first | `Commit` requires state `PREPARED`; `CAPTURED → SETTLED` is not an edge of the state machine | `TestCommit_WithoutPrepare_Fails` |
| T1.3 | A party sits on a prepared transfer and commits later at a stale FX rate | `Commit` compares `expiresAt` against `stub.GetTxTimestamp()` and refuses once passed | `TestCommit_AfterExpiry_Fails` |
| T1.4 | Replaying a transfer to double-pay a payee | `transferID` is the idempotency key; `CreateTransfer` rejects an ID already in state | `TestCreateTransfer_DuplicateID_Rejected` |
| T1.5 | Double-spend by preparing two transfers against the same funds | Locks are taken at `Prepare` and available balance is `total - locked` | `TestPrepare_InsufficientAvailable_Fails` |
| T1.6 | Funds stranded in `locked` forever after a counterparty disappears | `Abort` is callable by either party once `expiresAt` has passed, releasing both locks | `TestAbort_AfterExpiry_RefundsBothLegs` |
| T1.7 | Rounding or precision loss skimming value from each transfer | Every amount is an integer in minor units; rates are integer numerator / denominator; no float type appears in the contract | `TestAmounts_IntegerExact` |

## T2 — Authorisation

| # | Threat | Control | Test |
|---|---|---|---|
| T2.1 | An unrelated organisation commits someone else's transfer | Caller's MSP ID is read from the signing identity and checked against the transfer's two parties on every privileged call | `TestCommit_UnauthorizedOrg_Fails` |
| T2.2 | One org unilaterally settles without the counterparty agreeing | Endorsement policy requires both parties — `AND('Org1MSP.peer','Org2MSP.peer')` with the Phase 0 org mapping; a single-org transaction fails validation at the Validation Service | **Verified on the live network** — `network/demo.sh` commits with one org's endorsement and the transfer stays `PREPARED` |
| T2.3 | A client forges an MSP ID in the call arguments | MSP ID is never taken from arguments; it comes from `stub.GetCreator()` via the client identity library | `TestCommit_UnauthorizedOrg_Fails` |
| T2.4 | An operator raises their own limit and moves pool funds | Maker-checker: proposer must differ from approver for limit-exceeding actions | **Planned** — Phase 1 `policy` contract |

## T3 — The AI agent

| # | Threat | Control | Test |
|---|---|---|---|
| T3.1 | A mis-forecast or a bug in the agent moves real money | The agent holds no key that can move funds. It submits **proposals**; the `policy` contract decides | **Planned** — Phase 1 |
| T3.2 | A prompt-injected or manipulated LLM authorises a transfer | The LLM layer is explanation-only. It performs no arithmetic and signs no transaction | **Planned** — Phase 1 |
| T3.3 | The agent drains the pool through many small in-limit actions | Policy enforces a minimum pool balance and a maximum auto-movement per window, cumulative, not per call | **Planned** — Phase 1 adversarial suite |
| T3.4 | Agent decisions cannot be reconstructed after the fact | Every proposal, the inputs it saw and the policy verdict are written to an on-ledger audit log | **Planned** — Phase 1 |

## T4 — Compliance and privacy

| # | Threat | Control | Test |
|---|---|---|---|
| T4.1 | Personal data written to an immutable ledger, which cannot be erased | No PII on-chain at all. Only salted hashes, statuses and amounts. PII stays in an encrypted off-chain vault | Review rule, enforced in code review; **Planned** automated check |
| T4.2 | A hash of a known identifier is brute-forced from ledger state | Attestation hashes are salted per subject, and the salt lives only in the off-chain vault | **Planned** — Phase 1 `attestation` contract |
| T4.3 | Settlement runs for a sanctioned or unverified party | Settlement checks attestation validity and expiry **in the same transaction** as the transfer | **Planned** — Phase 1 |
| T4.4 | A stale KYC result is reused indefinitely | Attestations carry an issuer and an expiry; an expired attestation fails the settlement check | **Planned** — Phase 1 |

## T5 — Market and oracle

| # | Threat | Control | Test |
|---|---|---|---|
| T5.1 | A forged FX quote moves value to an attacker | Quotes are signed by the oracle, carry an expiry and a slippage bound, and are referenced by `quoteID` on the transfer | **Planned** — Phase 1 `fx-oracle` |
| T5.2 | A stale quote is replayed after the rate moves | Quote expiry is bounded by, and no later than, the transfer's own `expiresAt` | Partially covered by `TestCommit_AfterExpiry_Fails` |

## T6 — Platform and operations

| # | Threat | Control | Test |
|---|---|---|---|
| T6.1 | Endorsing peers disagree because chaincode is non-deterministic | No wall-clock time, no randomness, no map iteration in outputs, no external calls. Timestamps come from `stub.GetTxTimestamp()` | Review rule; divergence would surface as MVCC/validation failure on the live network |
| T6.2 | A modified upstream dependency is shipped without anyone noticing | Upstream DRUNIX is never vendored or modified. `network/` only wraps its scripts, and the commit we tested against is recorded | `network/README.md` records the exact upstream commit |
| T6.3 | Concurrent transfers on the same balance key silently overwrite each other | DRUNIX's Committing Peer runs MVCC validation; the losing transaction is marked invalid and must be resubmitted | **Planned** — concurrency test on the live network |

## Known gaps in Phase 0

Stated plainly, because the whole point of this document is that we do not
overclaim:

1. **There is one ledger, not two.** Cross-chain proof verification is stubbed as a
   local proof hash. The Herstatt-risk argument is sound for the state machine but
   has not been shown across two networks.
2. **No PII handling exists yet to audit**, because no service writes any. The rule
   is documented and will be enforceable once the compliance service lands.
3. **Nothing has been benchmarked.** No TPS or latency number appears anywhere in
   this repository, and none will until Caliper runs — and it cannot be measured
   honestly on the machine this was built on, because DRUNIX publishes amd64
   images only and everything here ran emulated on Apple silicon.
4. **The endorsement policy is chaincode-wide, not per-function.** T2.2 is
   verified, but Fabric applies the policy to every function rather than only to
   `Commit`. Narrowing it needs state-based endorsement, which is Phase 1. Erring
   toward the stricter policy is the safe direction.
5. **`PlatformOrg` does not exist on the network.** T2.1's test uses a
   non-party MSP ID against a mock stub; on the live two-org network there is no
   third organisation to try it with.
