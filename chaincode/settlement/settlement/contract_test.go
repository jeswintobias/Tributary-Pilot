package settlement

import (
	"encoding/json"
	"strings"
	"testing"
)

// The running example from the brief, in integer minor units.
//
//	Ravi sends 2,200 AED  = 220000 fils
//	Payout leg  ₹30,000    = 3000000 paise  (the college-fee portion)
const (
	senderOrg = "SenderOrgMSP"
	payoutOrg = "PayoutBankOrgMSP"
	otherOrg  = "PlatformOrgMSP"

	aed = "AED"
	inr = "INR"

	senderLeg uint64 = 220_000   // fils
	payoutLeg uint64 = 3_000_000 // paise

	// payeeRef stands in for a salted hash of the beneficiary reference,
	// computed off-chain. No PII reaches the ledger.
	payeeRef = "sha256:8f14e45fceea167a5a36dedd4bea2543c0ffee"

	quoteID    = "quote-2026-10-24-0001"
	transferID = "TP-UAE-IN-0001"

	t0        int64 = 1_761_300_000 // simulated tx time
	expiresAt int64 = t0 + 900      // 15-minute quote validity, the maximum window
)

// fund seeds both payers with comfortable balances and returns the ledger.
func fundedLedger(t *testing.T) *ledger {
	t.Helper()
	l := newLedger(t, t0)
	if err := l.seed(senderOrg, aed, 1_000_000); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if err := l.seed(payoutOrg, inr, 10_000_000); err != nil {
		t.Fatalf("seed payout bank: %v", err)
	}
	return l
}

func create(t *testing.T, l *ledger) error {
	t.Helper()
	return l.create(senderOrg, transferID, senderLeg, aed,
		payoutLeg, inr, quoteID, expiresAt, payoutOrg, payeeRef)
}

func mustCreate(t *testing.T, l *ledger) {
	t.Helper()
	if err := create(t, l); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
}

func mustAccept(t *testing.T, l *ledger) {
	t.Helper()
	if err := l.accept(payoutOrg, transferID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func mustPrepare(t *testing.T, l *ledger) {
	t.Helper()
	if err := l.prepare(senderOrg, transferID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}

// readyToCommit drives a transfer all the way to PREPARED.
func readyToCommit(t *testing.T, l *ledger) {
	t.Helper()
	mustCreate(t, l)
	mustAccept(t, l)
	mustPrepare(t, l)
}

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected an error containing %q, got %q", want, err.Error())
	}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestCommit_HappyPath_BothLegsMove(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	if got := l.transfer(transferID).State; got != StateCaptured {
		t.Fatalf("after CreateTransfer state = %s, want %s", got, StateCaptured)
	}

	mustAccept(t, l)
	if got := l.transfer(transferID).State; got != StateAccepted {
		t.Fatalf("after Accept state = %s, want %s", got, StateAccepted)
	}
	// Acceptance is consent to the terms, not a movement of value.
	if got := l.balance(payoutOrg, inr).Locked; got != 0 {
		t.Fatalf("Accept locked %d; it must lock nothing", got)
	}

	mustPrepare(t, l)

	tr := l.transfer(transferID)
	if tr.State != StatePrepared {
		t.Fatalf("after Prepare state = %s, want %s", tr.State, StatePrepared)
	}
	if tr.ProofHash == "" {
		t.Fatal("Prepare did not anchor a proof hash")
	}

	// Both legs are locked but nothing has moved yet.
	if got := l.balance(senderOrg, aed); got.Total != 1_000_000 || got.Locked != senderLeg {
		t.Fatalf("sender after Prepare = %+v, want total 1000000 locked %d", got, senderLeg)
	}
	if got := l.balance(payoutOrg, inr); got.Total != 10_000_000 || got.Locked != payoutLeg {
		t.Fatalf("payout bank after Prepare = %+v, want total 10000000 locked %d", got, payoutLeg)
	}
	if got := l.balance(payeeRef, inr); got.Total != 0 {
		t.Fatalf("payee credited before Commit: %+v", got)
	}

	// Commit is submitted by the counterparty, which is the realistic direction:
	// the payout bank releases once it can see the prepared sender leg.
	if err := l.commit(payoutOrg, transferID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := l.transfer(transferID).State; got != StateSettled {
		t.Fatalf("after Commit state = %s, want %s", got, StateSettled)
	}

	// Sender leg: SenderOrg -> PayoutBankOrg in AED.
	if got := l.balance(senderOrg, aed); got.Total != 1_000_000-senderLeg || got.Locked != 0 {
		t.Fatalf("sender after Commit = %+v, want total %d locked 0", got, 1_000_000-senderLeg)
	}
	if got := l.balance(payoutOrg, aed); got.Total != senderLeg {
		t.Fatalf("payout bank AED after Commit = %+v, want total %d", got, senderLeg)
	}
	// Payout leg: PayoutBankOrg -> beneficiary in INR.
	if got := l.balance(payoutOrg, inr); got.Total != 10_000_000-payoutLeg || got.Locked != 0 {
		t.Fatalf("payout bank INR after Commit = %+v, want total %d locked 0", got, 10_000_000-payoutLeg)
	}
	if got := l.balance(payeeRef, inr); got.Total != payoutLeg {
		t.Fatalf("payee after Commit = %+v, want total %d", got, payoutLeg)
	}

	// An event was emitted for every state change.
	for _, name := range []string{EventCaptured, EventAccepted, EventPrepared, EventSettled} {
		if l.lastEvent(name) == nil {
			t.Errorf("no %s event emitted", name)
		}
	}
	var ev stateChangeEvent
	if err := json.Unmarshal(l.lastEvent(EventSettled), &ev); err != nil {
		t.Fatalf("decode settled event: %v", err)
	}
	if ev.TransferID != transferID || ev.State != StateSettled || ev.PayoutLegAmount != payoutLeg {
		t.Fatalf("settled event = %+v", ev)
	}
}

// ---------------------------------------------------------------------------
// T1.2 — commit without prepare
// ---------------------------------------------------------------------------

func TestCommit_WithoutPrepare_Fails(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)
	mustAccept(t, l)

	err := l.commit(payoutOrg, transferID)
	requireErrContains(t, err, "Commit requires PREPARED")

	if got := l.transfer(transferID).State; got != StateAccepted {
		t.Fatalf("state changed to %s on a rejected Commit", got)
	}
	if got := l.balance(payeeRef, inr); got.Total != 0 {
		t.Fatalf("payee was credited by a rejected Commit: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T1.3 — commit after expiry
// ---------------------------------------------------------------------------

func TestCommit_AfterExpiry_Fails(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	l.at(expiresAt) // exactly at expiry: the boundary is inclusive-refusing
	requireErrContains(t, l.commit(payoutOrg, transferID), "expired")

	l.at(expiresAt + 3600)
	requireErrContains(t, l.commit(payoutOrg, transferID), "expired")

	if got := l.transfer(transferID).State; got != StatePrepared {
		t.Fatalf("state = %s after refused Commit, want %s", got, StatePrepared)
	}
	// Funds are still locked, not moved — the transfer must now be aborted.
	if got := l.balance(senderOrg, aed); got.Total != 1_000_000 || got.Locked != senderLeg {
		t.Fatalf("sender balance disturbed by refused Commit: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T1.4 — replay
// ---------------------------------------------------------------------------

func TestCreateTransfer_DuplicateID_Rejected(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	requireErrContains(t, create(t, l), "already exists")

	// Also rejected once the first one has settled: the ID is not recycled.
	mustAccept(t, l)
	mustPrepare(t, l)
	if err := l.commit(payoutOrg, transferID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	requireErrContains(t, create(t, l), "already exists")

	if got := l.balance(payeeRef, inr); got.Total != payoutLeg {
		t.Fatalf("payee paid %d, want exactly one payout of %d", got.Total, payoutLeg)
	}
}

// ---------------------------------------------------------------------------
// T1.5 — double-spend through a second prepare
// ---------------------------------------------------------------------------

func TestPrepare_InsufficientAvailable_Fails(t *testing.T) {
	l := newLedger(t, t0)
	// Exactly one sender leg's worth of AED, and plenty of INR.
	if err := l.seed(senderOrg, aed, senderLeg); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if err := l.seed(payoutOrg, inr, 10_000_000); err != nil {
		t.Fatalf("seed payout bank: %v", err)
	}

	readyToCommit(t, l)

	// A second transfer against the same funds must not be preparable.
	second := transferID + "-B"
	if err := l.create(senderOrg, second, senderLeg, aed,
		payoutLeg, inr, quoteID, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer second: %v", err)
	}
	if err := l.accept(payoutOrg, second); err != nil {
		t.Fatalf("Accept second: %v", err)
	}

	requireErrContains(t, l.prepare(senderOrg, second), "available")

	// The first transfer's lock is intact and the second took nothing.
	if got := l.balance(senderOrg, aed); got.Total != senderLeg || got.Locked != senderLeg {
		t.Fatalf("sender = %+v, want total %d locked %d", got, senderLeg, senderLeg)
	}
	if got := l.transfer(second).State; got != StateAccepted {
		t.Fatalf("second transfer state = %s, want %s", got, StateAccepted)
	}
}

// ---------------------------------------------------------------------------
// T1.6 — abort refunds both legs
// ---------------------------------------------------------------------------

func TestAbort_AfterExpiry_RefundsBothLegs(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	l.at(expiresAt + 1)
	if err := l.abort(payoutOrg, transferID); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if got := l.transfer(transferID).State; got != StateAborted {
		t.Fatalf("state = %s, want %s", got, StateAborted)
	}

	// Full refund: totals untouched, locks released, payee never credited.
	if got := l.balance(senderOrg, aed); got.Total != 1_000_000 || got.Locked != 0 {
		t.Fatalf("sender after Abort = %+v, want total 1000000 locked 0", got)
	}
	if got := l.balance(payoutOrg, inr); got.Total != 10_000_000 || got.Locked != 0 {
		t.Fatalf("payout bank after Abort = %+v, want total 10000000 locked 0", got)
	}
	if got := l.balance(payeeRef, inr); got.Total != 0 {
		t.Fatalf("payee credited by an Abort: %+v", got)
	}

	// Terminal: it cannot then be committed.
	requireErrContains(t, l.commit(payoutOrg, transferID), "Commit requires PREPARED")
}

func TestAbort_BeforeAnyLock_NeedsNoExpiry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept bool
		state  string
	}{
		{"captured", false, StateCaptured},
		{"accepted", true, StateAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := fundedLedger(t)
			mustCreate(t, l)
			if tc.accept {
				mustAccept(t, l)
			}
			if got := l.transfer(transferID).State; got != tc.state {
				t.Fatalf("state = %s, want %s", got, tc.state)
			}

			// Nothing is locked yet, so either party may walk away immediately.
			if err := l.abort(senderOrg, transferID); err != nil {
				t.Fatalf("Abort of a %s transfer: %v", tc.state, err)
			}
			if got := l.transfer(transferID).State; got != StateAborted {
				t.Fatalf("state = %s, want %s", got, StateAborted)
			}
			if got := l.balance(senderOrg, aed); got.Locked != 0 || got.Total != 1_000_000 {
				t.Fatalf("sender disturbed by the abort: %+v", got)
			}
		})
	}
}

func TestAbort_PreparedBeforeExpiry_Refused(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	// Unilateral early abort would strand the counterparty, which has locked
	// funds in good faith. See the Abort doc comment.
	requireErrContains(t, l.abort(senderOrg, transferID), "unilateral early abort is refused")

	if got := l.transfer(transferID).State; got != StatePrepared {
		t.Fatalf("state = %s, want %s", got, StatePrepared)
	}
}

// ---------------------------------------------------------------------------
// T2.1 / T2.3 — authorisation
// ---------------------------------------------------------------------------

func TestCommit_UnauthorizedOrg_Fails(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	requireErrContains(t, l.commit(otherOrg, transferID), "is not a party to transfer")

	if got := l.transfer(transferID).State; got != StatePrepared {
		t.Fatalf("a non-party moved the transfer to %s", got)
	}
	if got := l.balance(payeeRef, inr); got.Total != 0 {
		t.Fatalf("a non-party caused a payout: %+v", got)
	}
}

func TestPrepareAndAbort_UnauthorizedOrg_Fails(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)
	mustAccept(t, l)

	requireErrContains(t, l.prepare(otherOrg, transferID), "is not a party to transfer")

	mustPrepare(t, l)
	l.at(expiresAt + 1)
	requireErrContains(t, l.abort(otherOrg, transferID), "is not a party to transfer")
}

func TestCreateTransfer_SenderOrgComesFromIdentity(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	// SenderOrg is read from the signing identity, never from an argument, so a
	// client cannot name a different payer for the sender leg.
	if got := l.transfer(transferID).SenderOrg; got != senderOrg {
		t.Fatalf("senderOrg = %s, want %s", got, senderOrg)
	}

	// And an org cannot name itself as both parties.
	err := l.create(senderOrg, transferID+"-self", senderLeg, aed,
		payoutLeg, inr, quoteID, expiresAt, senderOrg, payeeRef)
	requireErrContains(t, err, "must differ")
}

// ---------------------------------------------------------------------------
// T1.7 — integer exactness
// ---------------------------------------------------------------------------

func TestAmounts_IntegerExact(t *testing.T) {
	// An amount far beyond float64's 2^53 exact-integer range. If any part of the
	// pipeline went through a float, the last digits would be lost.
	const huge uint64 = 9_007_199_254_740_993 // 2^53 + 1

	l := newLedger(t, t0)
	if err := l.seed(senderOrg, aed, huge); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if err := l.seed(payoutOrg, inr, huge); err != nil {
		t.Fatalf("seed payout bank: %v", err)
	}
	if got := l.balance(senderOrg, aed).Total; got != huge {
		t.Fatalf("seeded %d, read back %d — precision was lost", huge, got)
	}

	if err := l.create(senderOrg, transferID, huge, aed, huge, inr,
		quoteID, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	mustAccept(t, l)
	mustPrepare(t, l)
	if err := l.commit(payoutOrg, transferID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := l.balance(senderOrg, aed); got.Total != 0 {
		t.Fatalf("sender left with %d, want 0 — the move was not exact", got.Total)
	}
	if got := l.balance(payoutOrg, aed); got.Total != huge {
		t.Fatalf("payout bank received %d, want %d", got.Total, huge)
	}
	if got := l.balance(payeeRef, inr); got.Total != huge {
		t.Fatalf("payee received %d, want %d", got.Total, huge)
	}
}

func TestAmounts_ZeroLegsRejected(t *testing.T) {
	l := fundedLedger(t)

	err := l.create(senderOrg, "z1", 0, aed, payoutLeg, inr, quoteID, expiresAt, payoutOrg, payeeRef)
	requireErrContains(t, err, "senderLegAmount must be greater than zero")

	err = l.create(senderOrg, "z2", senderLeg, aed, 0, inr, quoteID, expiresAt, payoutOrg, payeeRef)
	requireErrContains(t, err, "payoutLegAmount must be greater than zero")
}

func TestSeedBalance_Overflow_Rejected(t *testing.T) {
	l := newLedger(t, t0)
	const max uint64 = 1<<64 - 1
	if err := l.seed(senderOrg, aed, max); err != nil {
		t.Fatalf("seed max: %v", err)
	}
	requireErrContains(t, l.seed(senderOrg, aed, 1), "overflow")
	if got := l.balance(senderOrg, aed).Total; got != max {
		t.Fatalf("balance wrapped to %d", got)
	}
}

// ---------------------------------------------------------------------------
// Other guards
// ---------------------------------------------------------------------------

func TestCommit_Twice_Fails(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)
	if err := l.commit(payoutOrg, transferID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	requireErrContains(t, l.commit(payoutOrg, transferID), "Commit requires PREPARED")

	if got := l.balance(payeeRef, inr); got.Total != payoutLeg {
		t.Fatalf("payee paid %d after a double Commit, want %d", got.Total, payoutLeg)
	}
}

func TestPrepare_Twice_Fails(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	requireErrContains(t, l.prepare(senderOrg, transferID), "Prepare requires ACCEPTED")

	// The second Prepare must not have doubled the lock.
	if got := l.balance(senderOrg, aed).Locked; got != senderLeg {
		t.Fatalf("locked = %d after a second Prepare, want %d", got, senderLeg)
	}
}

func TestPrepare_AfterExpiry_Fails(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)
	mustAccept(t, l)

	l.at(expiresAt + 1)
	requireErrContains(t, l.prepare(senderOrg, transferID), "expired")

	if got := l.balance(senderOrg, aed).Locked; got != 0 {
		t.Fatalf("an expired Prepare locked %d", got)
	}
}

func TestAccept_AfterExpiry_Fails(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	l.at(expiresAt + 1)
	requireErrContains(t, l.accept(payoutOrg, transferID), "expired")

	if got := l.transfer(transferID).State; got != StateCaptured {
		t.Fatalf("state = %s after an expired Accept, want %s", got, StateCaptured)
	}
}

func TestCreateTransfer_ExpiryInThePast_Rejected(t *testing.T) {
	l := fundedLedger(t)
	err := l.create(senderOrg, "past", senderLeg, aed, payoutLeg, inr,
		quoteID, t0-1, payoutOrg, payeeRef)
	requireErrContains(t, err, "not in the future")
}

func TestGetTransfer_Unknown_Fails(t *testing.T) {
	l := fundedLedger(t)
	_, err := l.sc.GetTransfer(l.as(senderOrg), "nope")
	requireErrContains(t, err, "does not exist")
}

func TestGetBalance_UnknownAccount_ReadsZero(t *testing.T) {
	l := newLedger(t, t0)
	b, err := l.sc.GetBalance(l.as(senderOrg), "never-seen", inr)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if b.Total != 0 || b.Locked != 0 {
		t.Fatalf("unknown account = %+v, want zeroes", *b)
	}
}

func TestCommit_TamperedTerms_FailProofCheck(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	// Simulate state tampering between prepare and commit by rewriting the
	// stored transfer with a larger payout leg while keeping the anchored hash.
	key, _ := l.as(senderOrg).GetStub().CreateCompositeKey(transferKeyPrefix, []string{transferID})
	tr := l.transfer(transferID)
	tr.PayoutLegAmount = payoutLeg * 2
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	l.stub.committed[key] = raw

	requireErrContains(t, l.commit(payoutOrg, transferID), "proof hash mismatch")
}

func TestProofHash_IsDeterministicAndFieldSensitive(t *testing.T) {
	base := Transfer{
		TransferID: transferID, SenderOrg: senderOrg, SenderLegAmount: senderLeg, SenderCcy: aed,
		PayoutOrg: payoutOrg, PayoutLegAmount: payoutLeg, PayoutCcy: inr, PayoutPayeeRef: payeeRef,
		QuoteID: quoteID, ExpiresAt: expiresAt, CreatedAt: t0, AcceptedAt: t0 + 1, PreparedAt: t0 + 2,
	}
	if proofHash(base) != proofHash(base) {
		t.Fatal("proofHash is not deterministic")
	}

	// Every economic field must change the digest.
	for name, mutate := range map[string]func(*Transfer){
		"PayoutLegAmount": func(x *Transfer) { x.PayoutLegAmount++ },
		"SenderLegAmount": func(x *Transfer) { x.SenderLegAmount++ },
		"PayoutPayeeRef":  func(x *Transfer) { x.PayoutPayeeRef += "x" },
		"AcceptedAt":      func(x *Transfer) { x.AcceptedAt++ },
		"ExpiresAt":       func(x *Transfer) { x.ExpiresAt++ },
	} {
		changed := base
		mutate(&changed)
		if proofHash(changed) == proofHash(base) {
			t.Errorf("proofHash ignores %s", name)
		}
	}

	// Length-prefixing must stop adjacent fields from being re-sliced into the
	// same digest: ("ab","c") and ("a","bc") are different transfers.
	x, y := base, base
	x.SenderCcy, x.PayoutOrg = "ab", "c"
	y.SenderCcy, y.PayoutOrg = "a", "bc"
	if proofHash(x) == proofHash(y) {
		t.Fatal("proofHash is ambiguous across adjacent fields")
	}
}

func TestSeedBalance_CreditsOnlyTheCallersOwnAccount(t *testing.T) {
	l := newLedger(t, t0)
	if err := l.seed(senderOrg, aed, 500); err != nil {
		t.Fatalf("SeedBalance: %v", err)
	}
	if got := l.balance(senderOrg, aed).Total; got != 500 {
		t.Fatalf("caller balance = %d, want 500", got)
	}
	// There is no argument by which the caller could have credited another org.
	if got := l.balance(payoutOrg, aed).Total; got != 0 {
		t.Fatalf("another org was credited: %d", got)
	}
}

// Two prepared transfers that each fit inside the available balance must both
// commit — the lock accounting should not be over-eager.
func TestPrepare_TwoTransfersWithinBalance_BothSettle(t *testing.T) {
	l := fundedLedger(t)
	readyToCommit(t, l)

	second := transferID + "-B"
	if err := l.create(senderOrg, second, senderLeg, aed,
		payoutLeg, inr, quoteID, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer second: %v", err)
	}
	if err := l.accept(payoutOrg, second); err != nil {
		t.Fatalf("Accept second: %v", err)
	}
	if err := l.prepare(senderOrg, second); err != nil {
		t.Fatalf("Prepare second: %v", err)
	}
	if got := l.balance(senderOrg, aed).Locked; got != 2*senderLeg {
		t.Fatalf("locked = %d, want %d", got, 2*senderLeg)
	}

	if err := l.commit(payoutOrg, transferID); err != nil {
		t.Fatalf("Commit first: %v", err)
	}
	if err := l.commit(payoutOrg, second); err != nil {
		t.Fatalf("Commit second: %v", err)
	}

	if got := l.balance(senderOrg, aed); got.Total != 1_000_000-2*senderLeg || got.Locked != 0 {
		t.Fatalf("sender = %+v", got)
	}
	if got := l.balance(payeeRef, inr).Total; got != 2*payoutLeg {
		t.Fatalf("payee = %d, want %d", got, 2*payoutLeg)
	}
}

func TestEmptyArguments_Rejected(t *testing.T) {
	l := fundedLedger(t)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"empty transferID", l.create(senderOrg, "", senderLeg, aed, payoutLeg, inr,
			quoteID, expiresAt, payoutOrg, payeeRef), "transferID must not be empty"},
		{"empty senderCcy", l.create(senderOrg, "e1", senderLeg, "", payoutLeg, inr,
			quoteID, expiresAt, payoutOrg, payeeRef), "senderCcy must not be empty"},
		{"empty quoteID", l.create(senderOrg, "e2", senderLeg, aed, payoutLeg, inr,
			"", expiresAt, payoutOrg, payeeRef), "quoteID must not be empty"},
		{"empty payeeRef", l.create(senderOrg, "e3", senderLeg, aed, payoutLeg, inr,
			quoteID, expiresAt, payoutOrg, ""), "payoutPayeeRef must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) { requireErrContains(t, tc.err, tc.want) })
	}
}
