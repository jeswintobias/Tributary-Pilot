package settlement

import (
	"strings"
	"testing"
)

// Regression tests for defects found in review. Each one failed against the
// code as it stood before the fix, with the mock stub reproducing Fabric's real
// transaction semantics: GetState does not see the current transaction's own
// PutState (txSimulator.SetState only appends to the write set, while
// queryExecutor.GetState reads the state database directly).

// A settling institution must never be named as the beneficiary. Before the
// fix, Commit debited the payout org and then re-read its stale balance to
// credit the "payee", discarding the debit and creating value out of nothing.
func TestCreateTransfer_PayeeIsAParty_Rejected(t *testing.T) {
	l := fundedLedger(t)

	err := l.create(senderOrg, "SELF-PAYOUT", senderLeg, aed,
		payoutLeg, inr, quoteID, expiresAt, payoutOrg, payoutOrg)
	requireErrContains(t, err, "must not be a party to the transfer")

	err = l.create(senderOrg, "SELF-SENDER", senderLeg, aed,
		payoutLeg, inr, quoteID, expiresAt, payoutOrg, senderOrg)
	requireErrContains(t, err, "must not be a party to the transfer")
}

// Both legs in the same currency touch the payout org's one balance twice:
// credited on the sender leg, debited on the payout leg. Before the fix the
// second leg read the pre-transaction value and the credit vanished.
func TestCommit_SameCurrencyBothLegs_IsExact(t *testing.T) {
	l := newLedger(t, t0)
	if err := l.seed(senderOrg, inr, 10_000_000); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if err := l.seed(payoutOrg, inr, 10_000_000); err != nil {
		t.Fatalf("seed payout bank: %v", err)
	}

	const senderAmt, payoutAmt uint64 = 4_000_000, 1_000_000
	if err := l.create(senderOrg, "SAMECCY", senderAmt, inr,
		payoutAmt, inr, quoteID, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := l.accept(payoutOrg, "SAMECCY"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := l.prepare(senderOrg, "SAMECCY"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := l.commit(payoutOrg, "SAMECCY"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Payout org: +4,000,000 from the sender leg, −1,000,000 on the payout leg.
	if got := l.balance(payoutOrg, inr); got.Total != 10_000_000+senderAmt-payoutAmt || got.Locked != 0 {
		t.Fatalf("payout bank = %+v, want total %d locked 0", got, 10_000_000+senderAmt-payoutAmt)
	}
	if got := l.balance(senderOrg, inr); got.Total != 10_000_000-senderAmt {
		t.Fatalf("sender = %+v, want total %d", got, 10_000_000-senderAmt)
	}
	if got := l.balance(payeeRef, inr); got.Total != payoutAmt {
		t.Fatalf("payee = %+v, want total %d", got, payoutAmt)
	}

	// Conservation: nothing created, nothing destroyed.
	total := l.balance(senderOrg, inr).Total + l.balance(payoutOrg, inr).Total + l.balance(payeeRef, inr).Total
	if total != 20_000_000 {
		t.Fatalf("total INR in the system is %d, want 20000000", total)
	}
}

// The sending org alone must not be able to lock the payout org's funds. Before
// the fix it could: CreateTransfer and Prepare were both callable by the sender,
// and expiresAt had no upper bound, so the lock could be held for years.
func TestPrepare_WithoutAccept_Fails(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	err := l.prepare(senderOrg, transferID)
	requireErrContains(t, err, "Prepare requires ACCEPTED")

	if got := l.balance(payoutOrg, inr).Locked; got != 0 {
		t.Fatalf("payout bank funds locked without its consent: %d", got)
	}
	if got := l.balance(senderOrg, aed).Locked; got != 0 {
		t.Fatalf("sender funds locked before acceptance: %d", got)
	}
}

func TestAccept_OnlyPayoutOrg(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	requireErrContains(t, l.accept(senderOrg, transferID), "only the payout org")
	requireErrContains(t, l.accept(otherOrg, transferID), "only the payout org")
	if got := l.transfer(transferID).State; got != StateCaptured {
		t.Fatalf("state = %s after refused Accept, want %s", got, StateCaptured)
	}

	if err := l.accept(payoutOrg, transferID); err != nil {
		t.Fatalf("Accept by payout org: %v", err)
	}
	tr := l.transfer(transferID)
	if tr.State != StateAccepted {
		t.Fatalf("state = %s, want %s", tr.State, StateAccepted)
	}
	if tr.AcceptedAt == 0 {
		t.Fatal("AcceptedAt was not recorded")
	}

	// Accept is not repeatable.
	requireErrContains(t, l.accept(payoutOrg, transferID), "Accept requires CAPTURED")
}

// An unbounded expiry would let an accepted transfer hold the counterparty's
// funds for as long as the sender chose, since a prepared transfer can only be
// aborted after it expires.
func TestCreateTransfer_ExpiryBeyondWindow_Rejected(t *testing.T) {
	l := fundedLedger(t)

	tenYears := t0 + 10*365*24*3600
	err := l.create(senderOrg, "LONG", 1, aed, 1, inr, quoteID, tenYears, payoutOrg, payeeRef)
	requireErrContains(t, err, "seconds ahead")

	// The boundary itself is allowed; one second past it is not.
	if err := l.create(senderOrg, "EDGE-OK", senderLeg, aed, payoutLeg, inr,
		quoteID, t0+MaxSettlementWindowSeconds, payoutOrg, payeeRef); err != nil {
		t.Fatalf("expiry exactly at the window should be allowed: %v", err)
	}
	err = l.create(senderOrg, "EDGE-NO", senderLeg, aed, payoutLeg, inr,
		quoteID, t0+MaxSettlementWindowSeconds+1, payoutOrg, payeeRef)
	requireErrContains(t, err, "seconds ahead")
}

// A failed call must leave nothing behind. Fabric discards the whole write set
// of a transaction whose chaincode returned an error, and the mock now does the
// same, so this asserts something real rather than something the mock permits.
func TestFailedCall_WritesNothing(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)
	if err := l.accept(payoutOrg, transferID); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := l.prepare(senderOrg, transferID); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	before := l.balance(senderOrg, aed)
	// A non-party commit fails after the party check and before any balance work.
	requireErrContains(t, l.commit(otherOrg, transferID), "is not a party to transfer")

	if got := l.balance(senderOrg, aed); got != before {
		t.Fatalf("a failed call changed state: %+v -> %+v", before, got)
	}
	if got := l.transfer(transferID).State; got != StatePrepared {
		t.Fatalf("state = %s after a failed call, want %s", got, StatePrepared)
	}
}

// ---------------------------------------------------------------------------
// Query functions used by the gateway and the operator views
// ---------------------------------------------------------------------------

func TestListTransfers_ReturnsEveryTransferInKeyOrder(t *testing.T) {
	l := fundedLedger(t)

	if got, err := l.sc.ListTransfers(l.as(senderOrg)); err != nil || len(got) != 0 {
		t.Fatalf("empty ledger: got %d transfers, err %v — want 0 and no error", len(got), err)
	}

	for _, id := range []string{"TP-C", "TP-A", "TP-B"} {
		if err := l.create(senderOrg, id, senderLeg, aed,
			payoutLeg, inr, quoteID, expiresAt, payoutOrg, payeeRef); err != nil {
			t.Fatalf("CreateTransfer %s: %v", id, err)
		}
	}

	got, err := l.sc.ListTransfers(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListTransfers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d transfers, want 3", len(got))
	}
	// Key order, not insertion order — the same order a peer would return.
	for i, want := range []string{"TP-A", "TP-B", "TP-C"} {
		if got[i].TransferID != want {
			t.Fatalf("position %d is %s, want %s", i, got[i].TransferID, want)
		}
		if got[i].State != StateCaptured {
			t.Fatalf("%s state = %s, want %s", got[i].TransferID, got[i].State, StateCaptured)
		}
	}

	// It reflects state changes, not just creations.
	if err := l.accept(payoutOrg, "TP-A"); err != nil {
		t.Fatalf("Accept TP-A: %v", err)
	}
	got, _ = l.sc.ListTransfers(l.as(senderOrg))
	if got[0].State != StateAccepted {
		t.Fatalf("TP-A state = %s after Accept, want %s", got[0].State, StateAccepted)
	}
}

func TestListBalances_ReturnsSeededAccounts(t *testing.T) {
	l := fundedLedger(t)

	got, err := l.sc.ListBalances(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListBalances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d balances, want 2", len(got))
	}

	byKey := map[string]uint64{}
	for _, b := range got {
		byKey[b.Account+"/"+b.Ccy] = b.Total
	}
	if byKey[senderOrg+"/"+aed] != 1_000_000 {
		t.Fatalf("sender AED = %d, want 1000000", byKey[senderOrg+"/"+aed])
	}
	if byKey[payoutOrg+"/"+inr] != 10_000_000 {
		t.Fatalf("payout INR = %d, want 10000000", byKey[payoutOrg+"/"+inr])
	}

	// A settled transfer adds the beneficiary's balance to the listing.
	readyToCommit(t, l)
	if err := l.commit(payoutOrg, transferID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, _ = l.sc.ListBalances(l.as(senderOrg))
	if len(got) != 4 {
		t.Fatalf("got %d balances after settlement, want 4 (sender AED, payout AED+INR, payee INR)", len(got))
	}
}

// The listing must not leak a transfer the caller is not party to being hidden
// — it is a full-ledger view by design, which is what a regulator observer
// needs. This records that intent so a future change does not silently alter it.
func TestListTransfers_IsAFullLedgerView(t *testing.T) {
	l := fundedLedger(t)
	mustCreate(t, l)

	got, err := l.sc.ListTransfers(l.as(otherOrg))
	if err != nil {
		t.Fatalf("ListTransfers as a non-party: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("non-party sees %d transfers, want 1 — this is a read-only ledger view", len(got))
	}
}

// ---------------------------------------------------------------------------
// Input bounds (Phase A hardening)
// ---------------------------------------------------------------------------

// Unbounded caller-controlled strings are written to every peer's disk and kept
// forever. The gateway validates too, but the CLI talks to the peer directly,
// so the contract has to be the one that holds.
func TestCreateTransfer_RejectsOversizedAndMalformedInput(t *testing.T) {
	l := fundedLedger(t)
	long := strings.Repeat("x", 300)

	for _, tc := range []struct {
		name, want                        string
		id, sCcy, pCcy, quote, org, payee string
	}{
		{"oversized transferID", "transferID is 300 bytes",
			long, aed, inr, quoteID, payoutOrg, payeeRef},
		{"oversized quoteID", "quoteID is 300 bytes",
			"ok-1", aed, inr, long, payoutOrg, payeeRef},
		{"oversized payeeRef", "payoutPayeeRef is 300 bytes",
			"ok-2", aed, inr, quoteID, payoutOrg, long},
		{"space in transferID", "contains ' '",
			"has space", aed, inr, quoteID, payoutOrg, payeeRef},
		{"newline in transferID", "contains '\\n'",
			"a\nb", aed, inr, quoteID, payoutOrg, payeeRef},
		{"lowercase currency", "must be three capital letters",
			"ok-3", "aed", inr, quoteID, payoutOrg, payeeRef},
		{"four-letter currency", "must be three letters",
			"ok-4", "AEDX", inr, quoteID, payoutOrg, payeeRef},
		{"empty payout currency", "must be three letters",
			"ok-5", aed, "", quoteID, payoutOrg, payeeRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := l.create(senderOrg, tc.id, senderLeg, tc.sCcy,
				payoutLeg, tc.pCcy, tc.quote, expiresAt, tc.org, tc.payee)
			requireErrContains(t, err, tc.want)
		})
	}

	// Nothing above was written.
	got, err := l.sc.ListTransfers(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListTransfers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("%d transfers were written by rejected calls", len(got))
	}
}

func TestCreateTransfer_AcceptsRealisticIdentifiers(t *testing.T) {
	l := fundedLedger(t)
	// The shapes the demo and the scripts actually use, including the colon in
	// a salted-hash reference and the dots an MSP ID may carry.
	if err := l.create(senderOrg, "TP-UAE-IN-1793876626", senderLeg, aed,
		payoutLeg, inr, "quote-2026-11-06.0001", expiresAt, payoutOrg,
		"sha256:8f14e45fceea167a5a36dedd4bea2543c0ffee"); err != nil {
		t.Fatalf("a realistic transfer was rejected: %v", err)
	}
	if got := l.transfer("TP-UAE-IN-1793876626").State; got != StateCaptured {
		t.Fatalf("state = %s, want %s", got, StateCaptured)
	}
}

func TestSeedBalance_RejectsMalformedCurrency(t *testing.T) {
	l := newLedger(t, t0)
	requireErrContains(t, l.seed(senderOrg, "aed", 100), "must be three capital letters")
	requireErrContains(t, l.seed(senderOrg, "RUPEES", 100), "must be three letters")
	requireErrContains(t, l.seed(senderOrg, "", 100), "must be three letters")
	if got := l.balance(senderOrg, aed).Total; got != 0 {
		t.Fatalf("a rejected seed credited %d", got)
	}
}
