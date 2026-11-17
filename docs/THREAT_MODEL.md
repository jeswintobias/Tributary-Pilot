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
| T1.10 | Value created or destroyed by a bug anywhere in the contract | Double-entry: one amount applied to both a debit and a credit, so an unbalanced entry cannot be expressed. Issuance and escrow are real accounts, so minting and locking balance too | `TestJournal_EveryStateChangeLeavesTheBooksBalanced` — and the check was verified by injecting a double-credit, which it caught |
| T1.11 | A payee poses as the issuer or as another transfer's escrow | `issuance:` and `escrow:` are reserved prefixes a caller-supplied payee may not use | `TestCreateTransfer_RejectsReservedAccountNames` |
| T1.8 | A transaction that writes a key twice loses one of the writes, creating or destroying value | Every balance operation in an invocation goes through one `balanceBook`, which caches this transaction's own writes. Fabric gives chaincode **no** read-your-writes: `SetState` appends to the write set while `GetState` reads the state DB | `TestCommit_SameCurrencyBothLegs_IsExact` — includes a conservation check |
| T1.9 | A settling institution names itself as the beneficiary and pays itself into existence | `CreateTransfer` rejects a `payoutPayeeRef` equal to either party's MSP ID | `TestCreateTransfer_PayeeIsAParty_Rejected` |
| T1.7 | Rounding or precision loss skimming value from each transfer | Every amount is an integer in minor units; rates are integer numerator / denominator; no float type appears in the contract | `TestAmounts_IntegerExact` |

## T2 — Authorisation

T2.1 is the one to read carefully. `PlatformOrg` (`Org3MSP`) is a **full channel
member** with Writer rights, so its transaction proposals are accepted by the
channel and executed by the chaincode. Nothing upstream of the contract turns
them away. On the live network, `Commit`, `Abort` and `Prepare` submitted with
`PlatformOrg`'s identity against a transfer between `SenderOrg` and
`PayoutBankOrg` are each refused with *"caller Org3MSP is not a party to
transfer"* — the contract's own check, not a channel ACL. In the same run it
reads the transfer successfully, which is the behaviour a regulator observer
should have.

| # | Threat | Control | Test |
|---|---|---|---|
| T2.1 | An unrelated organisation commits someone else's transfer | Caller's MSP ID is read from the signing identity and checked against the transfer's two parties on every privileged call | `TestCommit_UnauthorizedOrg_Fails`, and **verified on the live network** — `network/verify-platformorg.sh` |
| T2.2 | One org unilaterally settles without the counterparty agreeing | Endorsement policy requires both parties — `AND('Org1MSP.peer','Org2MSP.peer')` with the Phase 0 org mapping; a single-org transaction fails validation at the Validation Service | **Verified on the live network** — `network/demo.sh` commits with one org's endorsement and the transfer stays `PREPARED` |
| T2.3 | A client forges an MSP ID in the call arguments | MSP ID is never taken from arguments; it comes from `stub.GetCreator()` via the client identity library | `TestCommit_UnauthorizedOrg_Fails`, and live: `PlatformOrg` cannot name itself a party |
| T2.5 | The sending org locks the payout bank's liquidity without its consent | `Prepare` requires state `ACCEPTED`, and only the payout org may `Accept`. Nothing of the counterparty's can be locked until it has agreed to the terms | `TestPrepare_WithoutAccept_Fails`, `TestAccept_OnlyPayoutOrg`, and live in `network/demo.sh` |
| T2.6 | A transfer is created with a far-future expiry, holding locked funds hostage, since a prepared transfer can only be aborted after expiry | `expiresAt` may be at most `MaxSettlementWindowSeconds` (900s) past the transaction timestamp — the lifetime of a signed FX quote | `TestCreateTransfer_ExpiryBeyondWindow_Rejected` |
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

> Superseded in part by **T10.1**, which covers signed quotes as built. What
> remains below is the wider market risk the signing mechanism does not address.

| # | Threat | Control | Test |
|---|---|---|---|
| T5.1 | A forged FX quote moves value to an attacker | Quotes are signed by the oracle, carry an expiry and a slippage bound, and are referenced by `quoteID` on the transfer | **Planned** — Phase 1 `fx-oracle` |
| T5.2 | A stale quote is replayed after the rate moves | Quote expiry is bounded by, and no later than, the transfer's own `expiresAt` | Partially covered by `TestCommit_AfterExpiry_Fails` |

## T6 — Platform and operations

| # | Threat | Control | Test |
|---|---|---|---|
| T6.4 | A test mock is more permissive than Fabric, so a real defect passes CI | The mock stub reproduces Fabric's transaction semantics: writes are buffered and invisible to `GetState` within the same transaction, and discarded entirely when the call returns an error | `TestFailedCall_WritesNothing`; T1.8 and T1.9 were both found this way |
| T6.1 | Endorsing peers disagree because chaincode is non-deterministic | No wall-clock time, no randomness, no map iteration in outputs, no external calls. Timestamps come from `stub.GetTxTimestamp()` | Review rule; divergence would surface as MVCC/validation failure on the live network |
| T6.2 | A modified upstream dependency is shipped without anyone noticing | Upstream DRUNIX is never vendored or modified. `network/` only wraps its scripts, and the commit we tested against is recorded | `network/README.md` records the exact upstream commit |
| T6.3 | Concurrent transfers on the same balance key silently overwrite each other | DRUNIX's Committing Peer runs MVCC validation; the losing transaction is marked invalid and must be resubmitted. The netting path (T8) removes the shared key from the hot path rather than relying on retries | `TestNetting_TwoInstitutionsSubmitWithoutContending` compares actual read and write sets; a throughput measurement under concurrent load is still **planned** |

## T8 — The netting path

Netting records obligations that move no value until a window closes, which
creates a class of threat the two-phase commit does not have: value promised
but not yet moved.

| # | Threat | Control | Test |
|---|---|---|---|
| T8.1 | An institution promises the same funds twice, on the netting path and again through `Prepare` | Exposure is subtracted from a balance before **any** debit is allowed, on either path. A reservation is not a bookkeeping note; the journal enforces it | `TestExposure_BlocksTheTwoPhaseCommitFromSpendingCommittedFunds` |
| T8.2 | An institution commits its whole balance, waits for the window to roll, and commits it again | Exposure spans windows. It is keyed `expo:<ccy>:<account>` with no sequence number, so a past-but-unsettled window still counts against cover | `TestSubmitObligation_CoverCountsUnsettledWindowsTogether` |
| T8.3 | An obligation is submitted naming another institution as the debtor | The debtor is the signing MSP, read from the signature. There is no debtor argument, in the chaincode or in the gateway | `TestSubmitObligation_AttributesTheDebtToTheSigner` |
| T8.4 | A window is closed twice, settling the same obligations again | The window record is the idempotency key, as the transfer record is on the 2PC path. A second close is refused | `TestCloseWindow_RejectsAReplay`, and the live run |
| T8.5 | A window is closed while still accruing, so a phantom obligation is inserted into the range the close is reading | A window may only be closed once `seq` is strictly in the past, and a submission lands in the window its own timestamp names. The range is immutable before it is read | `TestCloseWindow_RefusesAWindowThatIsStillOpen`, and the live run |
| T8.6 | Endorsing peers compute different net positions, so every netted transaction fails validation | The fold sorts by account name and pairing breaks ties by name. Nothing reaches a write set in map order | `TestNetting_IsDeterministicWhateverTheObligationOrder`, `TestNetPositions_AreOrderedByAccount` |
| T8.7 | Whoever closes a window chooses a netting favourable to themselves | Netting is a pure function of state that is already frozen, so every caller computes byte-identical results. Any member may close; there is no privileged closer to compromise | `TestCloseWindow_AnyMemberClosesToTheSameResult` |
| T8.8 | A creditor names a reserved counter-account and is credited from the issuance liability | `issuance:` and `escrow:` prefixes are refused as creditors, as they are as payees | `TestSubmitObligation_Guards` |
| T8.9 | Netting settles the wrong figure while the books still balance | Double entry does **not** catch this — confirmed by injecting a doubled posting, which kept conservation intact. The invariant that catches it is that every account moves by exactly its net position | `TestNetting_EveryAccountMovesByExactlyItsNetPosition`, and the live run derives the expected net from committed obligations |

### Still open on the netting path

1. **Cover is gross, not net.** An institution must hold the full outgoing
   amount while a window is open, even if it nets to nothing. Netting
   compresses the value that *moves*, not the collateral held. Fixing it needs
   bilateral credit limits, which is a counterparty-risk decision rather than a
   refactor.
2. **One institution's submissions serialise on its own exposure record.**
   Parallelism is across institutions, not within one. Sharding that key is a
   contained change and is not pretended to be done.
3. **Nothing guarantees a window gets closed.** Any member *may* close one, but
   no member *must*. An unclosed window holds its participants' reservations
   indefinitely. A scheduled closer belongs in the Phase C service layer, and
   until it exists this is an operational dependency, not a protocol guarantee.
4. **A window's compression is visible to every channel member.** Obligations
   name both parties and amounts in the clear, so the corridor's bilateral flow
   is readable by anyone on the channel. Private data collections would fix it;
   no one has done that work.

## T10 — FX, compliance, policy and vouchers

The controls layer. Each of these closes a hole that was open and real until
it landed; the detail and the reasoning are in [CONTROLS.md](CONTROLS.md).

### T10.1 — The rate

| # | Threat | Control | Test |
|---|---|---|---|
| T10.1.1 | The sender names a rate nobody quoted | A transfer may only be created against a quote signed by a registered oracle | `TestQuote_UnsignedRateIsRefused`, `TestQuote_ForgedSignatureIsRefused`, and the live run |
| T10.1.2 | Yesterday's favourable rate is used today | The quote carries an expiry, checked against the transaction timestamp | `TestQuote_ExpiredRateIsRefused`, and the live run |
| T10.1.3 | A genuine signed quote is attached to legs that bear no relation to it | The payout leg must sit within `maxSlippageBps` of `senderAmount × rateNum ÷ rateDen` | `TestQuote_LegsThatDoNotMatchTheRateAreRefused`, and the live run |
| T10.1.4 | A signature over one quote verifies another | Fields are length-prefixed with a domain separator, so two quotes cannot serialise identically | `TestQuoteBytes_FieldsCannotBleedIntoEachOther` |
| T10.1.5 | The Go and JavaScript encodings drift apart, so every quote is rejected for a reason nobody can diagnose | A signed vector produced by the oracle service is re-derived and verified by the chaincode's own implementation | `TestQuote_MatchesTheOracleServiceEncoding`; verified to fail on a dropped length prefix and on a changed domain separator |
| T10.1.6 | A favourable quote is spent repeatedly as the market moves | A quote is single use, keyed by quote ID | `TestCreateTransfer_RefusesAReusedQuote` |
| T10.1.7 | The rate check overflows and silently passes anything | `mulDiv` keeps the intermediate product in 128 bits | `TestMulDiv`, including a product beyond 2^64 |
| T10.1.8 | A quote for another corridor is replayed | The quote's currency pair must match the transfer's | `TestQuote_WrongCurrencyPairIsRefused` |
| T10.1.9 | Anyone installs a trusted signing key | Only `OracleMSPs` may register, and a key cannot be silently replaced | `TestRegisterOracle_OnlyAnApprovedOrgMay`, and the live run |
| T10.1.10 | The rate is altered between prepare and commit | The proof hash covers the rate, not just the quote ID | `TestProofHash_CoversTheRate` |

### T10.2 — The beneficiary

| # | Threat | Control | Test |
|---|---|---|---|
| T10.2.1 | Value settles to somebody nobody identified or screened | Live KYC and sanctions attestations are required before the payout leg moves | `TestPrepare_RefusedWithoutAttestations`, and the live run |
| T10.2.2 | A sanctions hit lands after funds are locked and the payout goes through anyway | Attestations are checked at Prepare **and again at Commit** | `TestCommit_RefusedWhenSanctionsLandMidSettlement`; verified to fail when the Commit check is removed |
| T10.2.3 | An attestation expires mid-settlement and is still honoured | The same check uses the transaction timestamp at Commit | `TestCommit_RefusedWhenTheAttestationExpiresMidSettlement` |
| T10.2.4 | A party clears its own beneficiary | Only `AttestorMSPs` may issue; the issuer is the signing MSP, never an argument | `TestIssueAttestation_OnlyAnApprovedAttestorMay`, and the live run |
| T10.2.5 | One attestor invalidates another's work | Only the issuer may revoke | `TestRevokeAttestation_OnlyTheIssuerMay` |
| T10.2.6 | A sanctions hit is undone by writing a fresh attestation over it | Re-issuing over a revocation is refused outright | `TestIssueAttestation_WillNotPaperOverARevocation` |
| T10.2.7 | A check is performed once and trusted for ever | Validity is capped at a year | `TestIssueAttestation_RefusesAnUnboundedValidity` |
| T10.2.8 | A revocation strands the counterparty's locked funds | Refunds are not gated on compliance: value returning to its owner is not a payout | `TestAbort_RefundsEvenAfterRevocation` |
| T10.2.9 | Personal data reaches the ledger | The subject is the same salted hash the payout leg already carries; the struct has nowhere to put a name | `TestAttestation_NoPersonalDataReachesTheLedger` |

### T10.3 — The limits

| # | Threat | Control | Test |
|---|---|---|---|
| T10.3.1 | One compromised key raises every limit in the system | A policy change needs two **distinct** organisations; the proposer may not approve | `TestPolicy_ProposerCannotApproveItsOwnChange`; verified to fail when the check is removed |
| T10.3.2 | An approver is made to approve something other than what was proposed | The proposal is keyed by the hash of its own contents | `TestPolicy_ProposalIDIsTheHashOfItsContents` |
| T10.3.3 | Endorsing peers compute different proposal IDs, so nothing is ever approvable | Limit maps and member lists are sorted before hashing | `TestPolicy_HashIsDeterministicAcrossMapOrder` |
| T10.3.4 | A transfer far outside what the channel agreed to | Per-currency caps on transfers and obligations, checked at creation | `TestPolicy_CapsASingleTransfer`, `TestPolicy_CapsANettingObligation` |
| T10.3.5 | An organisation nobody approved becomes a counterparty | `ApprovedMembers`, when set, restricts who may be a party | `TestPolicy_RestrictsWhoMayBeAParty` |
| T10.3.6 | A transfer with a window too short for the counterparty to act | `MinSettlementWindowSeconds` floors it | `TestPolicy_FloorsTheSettlementWindow` |
| T10.3.7 | Deploying a policy mechanism bricks a working channel | An unset policy imposes nothing | `TestPolicy_UnsetImposesNothing` |

### T10.4 — Where the money goes afterwards

| # | Threat | Control | Test |
|---|---|---|---|
| T10.4.1 | Money said to be for one purpose is spent on another | Value leaves the beneficiary's balance at issue and can only reach the named recipients | `TestVoucher_BindingMovesValueOutOfTheBeneficiarysBalance` |
| T10.4.2 | A voucher binds part of a payout and leaves the rest unaccounted for | The splits must total the payout leg exactly | `TestVoucher_MustAccountForTheWholePayoutLeg`; verified to fail when the check is removed |
| T10.4.3 | A voucher routes value to somebody who could not have received it directly | Every recipient is screened at issue | `TestVoucher_EveryRecipientMustBeScreened` |
| T10.4.4 | A recipient sanctioned after issue is still paid | Screened again at redemption, because that is when the money moves | `TestVoucher_RedemptionIsRefusedAfterARevocation`; verified to fail when the check is removed |
| T10.4.5 | A split is claimed twice | Each split records its own redemption | `TestVoucher_ASplitCannotBeRedeemedTwice` |
| T10.4.6 | Value is bound for ever because a recipient never claims | Unredeemed value returns to the beneficiary after expiry | `TestVoucher_UnredeemedValueGoesBackOnExpiry` |
| T10.4.7 | A refund is taken before a recipient has had the chance to claim | Refunds are refused before expiry | `TestVoucher_CannotBeRefundedBeforeExpiry` |
| T10.4.8 | A voucher account is named as a recipient, to launder value between vouchers | `voucher:` joins `issuance:` and `escrow:` as a reserved prefix | `TestVoucher_RefusesAReservedRecipient` |

### Still open on the controls

1. **Every allow-list is a compile-time constant.** `OracleMSPs`,
   `AttestorMSPs` and `PolicyAdminMSPs` live in the chaincode, so changing any
   of them needs a redeploy. They belong in the channel configuration, where a
   change takes the ordinary config-update signatures. This is the weakest part
   of the design.
2. **The oracle's key is a file on disk.** Anyone holding it can sign any rate
   and the chaincode will believe it. Generated on first use, written `0600`,
   never committed — and it belongs in an HSM.
3. **Nothing caps slippage.** `maxSlippageBps` is the oracle's own statement so
   a client cannot widen it, but an oracle could quote a tolerance so wide it
   means nothing. A policy cap is obvious work and is not done.
4. **Attestations are per-beneficiary, not per-transaction.** Somebody cleared
   once is cleared until expiry or revocation. Per-transaction screening is a
   different and much heavier model.
5. **A voucher's purposes are free text.** `TUITION` is recorded, not enforced.
   The chaincode cannot tell whether the recipient is really a college.
6. **Policy has no velocity limits.** Per-transfer caps only. A daily
   cumulative limit needs a windowed counter, which is a shared key and would
   need the treatment the netting path got.
7. **A salted hash is only as private as its salt.** If it leaks, the hash of a
   known name can be recomputed and matched, and the ledger then reveals which
   beneficiaries transacted. The salt lives in the compliance service and never
   on chain.

## T9 — The projector, the queue and the payout worker

A service layer that reacts to the ledger can fail in ways the ledger cannot.
The ledger is consistent by construction; a process reading it is not, and the
interesting threats are about what happens between a block being committed and
somebody being paid.

| # | Threat | Control | Test |
|---|---|---|---|
| T9.1 | Events that occur while the projector is down are never seen | The event stream is resumed from a stored checkpoint, not from "now". Without `startBlock` the SDK starts at the next block and the gap is silent | `the checkpoint resumes from the last applied block, not the one after`, and the live restart in `demo-pipeline.sh` |
| T9.2 | A restart re-applies events, double-counting the read model | `applied_events` is keyed by (block, tx, event); a second application is a no-op | `an event applied twice changes the read model once`, `replaying a whole block after a restart loses nothing and duplicates nothing` |
| T9.3 | A crash between two writes leaves the read model, the checkpoint and the queue disagreeing | All three are one SQLite transaction. There is no dual write to go wrong | `a failure part-way through an event leaves no trace of it at all` |
| T9.4 | A settled transfer is never paid because the enqueue was lost | The job is written in the same transaction as the settlement it came from, so it exists if and only if the settlement was recorded | `a settled transfer enqueues exactly one payout, however many times it is seen` |
| T9.5 | A beneficiary is paid twice | Jobs carry a dedupe key derived from the transfer; the payout row is keyed by it; the rail reference is derived rather than generated, so a retry presents the same idempotency key | `a job delivered twice pays once`, `the rail reference is stable across attempts` |
| T9.6 | A crashed worker holds a job for ever | Jobs are leased with a deadline, not locked. An expired lease is reclaimed | `an abandoned lease is picked up by another worker` |
| T9.7 | One unpayable transfer blocks every payout behind it | Retries with exponential backoff, then the job is moved to `dead_letters` and leaves the queue | `a poison job does not block the work behind it` |
| T9.8 | A failed payout is quietly dropped | Dead letters are kept, and the payout is recorded as DEAD rather than absent. A payment that did not happen is a thing to look at | `a job that can never succeed is dead-lettered rather than retried for ever` |
| T9.9 | An amount above 2^53 is rounded between the ledger and the rail | Payloads are parsed with a reviver that keeps each integer's source text; amounts are stored and compared as text | `an amount beyond 2^53 survives the projection exactly`, `an amount beyond 2^53 reaches the rail exactly as the ledger recorded it` |
| T9.10 | A stale projection is mistaken for live data | `/api/pipeline` reports the lag in blocks against the ledger's real height, read from `qscc`, and the view states it. Unknown lag is shown as unknown, never as zero | Visible in the corridor view; the live run shows the projection catching up |
| T9.11 | An unfamiliar event halts the projector for ever | Unknown events advance the checkpoint and are counted as skipped rather than throwing | `an unknown event advances the checkpoint instead of halting the projector` |
| T9.12 | A late or replayed event drags a settled transfer back to an earlier state | Transfer states are ranked and the read model refuses to move backwards | `the read model never goes backwards` |
| T9.13 | The gateway corrupts the projection it reads | The gateway opens the store **read-only**; a write from that process is refused by SQLite | Verified: `attempt to write a readonly database` |

### Still open on the pipeline

1. **An event the projector cannot apply stops it.** That is deliberate — a
   hole in the read model should not be hidden behind every event after it —
   but it means one malformed event halts projection until someone intervenes.
   The checkpoint does not move, so the fix is to correct the cause and
   restart.
2. **Nothing re-drives a dead letter.** They accumulate for a human to look at.
   An operator action to requeue one is obvious work and is not built.
3. **No leader election.** Two projectors against one store would both apply
   the same events — correctly, thanks to T9.2, but pointlessly. Several
   workers are fine; that is what leases are for.
4. **The projection is never verified against the ledger.** Rebuilding it is
   the recovery path (delete the store, replay from block 0), but nothing
   periodically checks that the projection still agrees with the chain, which
   is what would catch a projection bug that only bites on one event shape.
5. **Backoff is not jittered.** Deterministic so tests are reproducible. With
   several workers they would retry in lockstep.
6. **The payout rail is simulated.** Labelled as such everywhere it appears. A
   real one needs a licensed counterparty.

## T7 — The service layer

Everything above concerns the chaincode. The gateway in `services/gateway` sits
in front of it, holds signing keys, and until Phase A had **no access control of
any kind**: it listened on every network interface, required no credential, and
took the organisation to act as from the request body. Anyone who could reach
the port could move the payout bank's money. Strong vault, open reception desk.

That is fixed. What follows is the state after Phase A, including the parts
still open.

| # | Threat | Control | Test |
|---|---|---|---|
| T7.1 | Anyone on the network reaches the service and moves money | Binds to `127.0.0.1` unless `GATEWAY_HOST` overrides it, and the startup banner warns when it does | Verified: refused on the LAN address, reachable on loopback |
| T7.2 | An unauthenticated caller submits transactions | Every endpoint except the public org roster requires a bearer token; 401 otherwise | Verified: no token and a wrong token both return 401, for reads and writes |
| T7.3 | A caller acts as an organisation it is not | **A token maps to exactly one MSP.** The identity comes from the token, never from the request. The `as` field was removed | Verified: a request from SenderOrg's token naming `Org2MSP` in the body credited `Org1MSP` |
| T7.4 | Money minted from nothing through `SeedBalance` | The endpoint returns 403 unless `GATEWAY_DEV=1` | Verified: 403 in the default posture |
| T7.5 | Request flooding jams the ledger through MVCC conflicts | Fixed window of 60 requests per 10s per token, `Retry-After` on 429 | Verified: 70 requests produced 53×200 and 17×429 |
| T7.6 | Unbounded input bloats every peer's disk | Length and charset bounds on every identifier, three-letter currency codes, integer-only amounts, 16KB body cap. **Enforced in the chaincode as well**, because the CLI never passes through the gateway | `TestCreateTransfer_RejectsOversizedAndMalformedInput`, plus HTTP 400 for each case |
| T7.7 | Amounts silently rounded in transit | Amounts travel as decimal strings and are parsed with `BigInt`. A `uint64` in minor units exceeds what a JavaScript double holds exactly | Verified: fractional and negative amounts rejected |
| T7.8 | A weak or default credential ships with the service | Tokens are generated with `crypto.randomBytes` on first boot and written `0600`. There is no default | Verified in the startup banner |

### Still open, and deliberately recorded

1. **No TLS on the service.** The browser hop is plain HTTP. Acceptable only
   while it is bound to loopback. Anything else needs a proxy terminating TLS.
2. **One gateway holds all three organisations' keys.** This is a
   single-operator demo convenience and would be indefensible in production,
   where each organisation runs its own gateway with its own key. Mitigated in
   spirit by T7.3 — a token cannot cross organisations — but the keys are
   still in one process.
3. **`SeedBalance` still exists in the chaincode.** The gateway refuses it, but
   any channel member can call it with the CLI. It mints only to the caller's
   own account, so the exposure is bounded, and it disappears with the Phase 1
   token contract.
4. **Signing keys sit unencrypted on disk.** Test-network material today.
   Production needs an HSM; that is the ₹8,000 line in the Phase 1 budget.
5. **Dependency supply chain.** `npm install` reports packages with install
   scripts, including one that compiles native code. Versions are pinned and a
   lockfile is committed; nothing audits them.
6. **No per-call audit trail in the service.** The ledger records every
   committed transaction, but the gateway does not correlate its own requests
   to transaction IDs, so a refused call leaves no durable record.
7. **The read path parses amounts as doubles — partly fixed.** Requests are
   scrupulous about this: amounts travel as decimal strings and are parsed with
   `BigInt`, because a `uint64` in minor units exceeds what a double holds
   exactly. The gateway's own passthrough endpoints were undoing it, running
   `JSON.parse` over the chaincode's reply so that a balance above 2^53 minor
   units would be rounded before the page saw it.
   **The projector path no longer does this** — see T9.9 — so anything read
   through the read model is exact. The gateway's direct-to-peer endpoints
   (`/api/state`, `/api/netting`, `/api/transfer`) still use `JSON.parse` and
   still have the gap. The same reviver would close it; the figures in play are
   nowhere near the bound, so nothing on screen today is wrong.

### The layers, and which exist

```
1. Network      who can reach the port?       ✅ loopback only
2. Service auth who is this caller?           ✅ bearer token -> one MSP
3. Channel      is this org on the network?   ✅
4. Endorsement  did enough banks agree?       ✅ AND(Org1, Org2)
5. Chaincode    is this caller allowed?       ✅
6. Validation   is the signed result valid?   ✅
7. MVCC         did state change underneath?  ✅
```

Before Phase A, layers 1 and 2 did not exist.

## Found in review, and fixed

Two defects were found when an external reviewer read the contract against
Fabric's real semantics, and both are worth recording rather than quietly
patching:

1. **No read-your-writes (T1.8, T1.9).** `Commit` settles two legs. Where both
   touch the same account and currency, the second leg read the balance as it
   was *before* the transaction and overwrote the first leg's write. Paying an
   account that had just been debited restored its old total — value created out
   of nothing. The reverse order destroyed value. The test mock applied writes
   immediately, so all 26 tests passed over it. The mock now matches Fabric and
   the contract routes every balance operation through one `balanceBook`.

2. **Unilateral locking (T2.5, T2.6).** `CreateTransfer` and `Prepare` were both
   callable by the sending org alone, and `expiresAt` had no upper bound. A
   one-fils transfer could lock the payout bank's entire balance for years, and
   the bank could not abort until expiry. `Accept` and the bounded settlement
   window close it — and restore the capture/**accept** phase the brief
   describes, which the first implementation had collapsed into one step.

## Known gaps in Phase 0

0. **The service layer carries its own risks**, listed in T7 above with six
   still open — TLS and the shared-key gateway being the two that matter most.


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
5. **`PlatformOrg` has no peers of its own.** It is a channel member with an
   admin identity and read access, which is what T2.1 needed and all a regulator
   observer requires. Its Lite, Committing and Validation peers are Phase 1.
