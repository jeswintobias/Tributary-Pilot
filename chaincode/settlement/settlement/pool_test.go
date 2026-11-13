package settlement

import (
	"fmt"
	"math"
	"testing"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// baseNow sits inside a netting window rather than on its boundary, so no test
// depends accidentally on the window rolling at a convenient moment.
const baseNow int64 = 1_700_000_020

// nettingLedger seeds the two settling institutions so they have cover to commit.
func nettingLedger(t *testing.T, amount uint64) *ledger {
	t.Helper()
	l := newLedger(t, baseNow)
	if err := l.seed(senderOrg, "AED", amount); err != nil {
		t.Fatalf("seed %s: %v", senderOrg, err)
	}
	if err := l.seed(payoutOrg, "AED", amount); err != nil {
		t.Fatalf("seed %s: %v", payoutOrg, err)
	}
	return l
}

// ---------------------------------------------------------------------------
// What netting is for
// ---------------------------------------------------------------------------

// Obligations that cancel exactly should settle by moving nothing at all. This
// is the case that makes the whole design worthwhile: a corridor with balanced
// two-way flow needs no settlement liquidity whatsoever.
func TestNetting_OffsettingObligationsSettleWithNoMovement(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	before := l.balance(senderOrg, "AED").Balance
	seq := l.seqNow()

	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 500_000); err != nil {
		t.Fatalf("submit OB-1: %v", err)
	}
	if err := l.submit(payoutOrg, "OB-2", senderOrg, "AED", 500_000); err != nil {
		t.Fatalf("submit OB-2: %v", err)
	}

	l.at(baseNow + NettingWindowSeconds)
	w, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}

	if w.Obligations != 2 {
		t.Errorf("Obligations = %d, want 2", w.Obligations)
	}
	if w.GrossValue != 1_000_000 {
		t.Errorf("GrossValue = %d, want 1000000", w.GrossValue)
	}
	if w.NetValue != 0 {
		t.Errorf("NetValue = %d, want 0: offsetting obligations must move nothing", w.NetValue)
	}
	if w.Postings != 0 {
		t.Errorf("Postings = %d, want 0", w.Postings)
	}
	if w.CompressionBps != 10000 {
		t.Errorf("CompressionBps = %d, want 10000 (total compression)", w.CompressionBps)
	}
	if got := l.balance(senderOrg, "AED").Balance; got != before {
		t.Errorf("balance moved from %d to %d; a fully netted window must leave it alone", before, got)
	}
	l.assertBooksBalance()
}

// A corridor with uneven two-way flow should settle only the residue, and the
// window record should say how much liquidity that saved.
func TestNetting_CompressesGrossFlowToTheResidue(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	senderBefore := l.balance(senderOrg, "AED").Balance
	payoutBefore := l.balance(payoutOrg, "AED").Balance
	seq := l.seqNow()

	// Five payments out, four back: the shape of a real remittance corridor.
	for i := 0; i < 5; i++ {
		if err := l.submit(senderOrg, fmt.Sprintf("OB-OUT-%d", i), payoutOrg, "AED", 100_000); err != nil {
			t.Fatalf("submit OB-OUT-%d: %v", i, err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := l.submit(payoutOrg, fmt.Sprintf("OB-IN-%d", i), senderOrg, "AED", 100_000); err != nil {
			t.Fatalf("submit OB-IN-%d: %v", i, err)
		}
	}

	l.at(baseNow + NettingWindowSeconds)
	w, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}

	if w.GrossValue != 900_000 {
		t.Errorf("GrossValue = %d, want 900000", w.GrossValue)
	}
	if w.NetValue != 100_000 {
		t.Errorf("NetValue = %d, want 100000", w.NetValue)
	}
	// 1 - 100000/900000 is 88.888...%, and the figure is floored rather than
	// rounded: a savings metric that rounds up overstates the saving, which is
	// the one direction it must never err in.
	if w.CompressionBps != 8888 {
		t.Errorf("CompressionBps = %d, want 8888", w.CompressionBps)
	}

	// The claim that matters for throughput: the shared balance keys are
	// written far fewer times than the unnetted path would write them.
	unnetted := 2 * w.Obligations
	if w.Postings >= unnetted {
		t.Errorf("Postings = %d, which is no better than the unnetted %d", w.Postings, unnetted)
	}
	if w.Postings != 1 {
		t.Errorf("Postings = %d, want 1: one residual movement settles the window", w.Postings)
	}

	if got := l.balance(senderOrg, "AED").Balance; got != senderBefore-100_000 {
		t.Errorf("%s balance = %d, want %d", senderOrg, got, senderBefore-100_000)
	}
	if got := l.balance(payoutOrg, "AED").Balance; got != payoutBefore+100_000 {
		t.Errorf("%s balance = %d, want %d", payoutOrg, got, payoutBefore+100_000)
	}
	l.assertBooksBalance()
}

// Multilateral netting should let value pass straight through an institution
// whose own position nets to zero, without that institution's balance moving or
// its account being written at all.
func TestNetting_PassesValueThroughAnInstitutionThatNetsToZero(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	middleBefore := l.balance(payoutOrg, "AED").Balance
	seq := l.seqNow()

	// Sender owes the middle institution, which owes the third the same amount.
	if err := l.submit(senderOrg, "OB-A", payoutOrg, "AED", 300_000); err != nil {
		t.Fatalf("submit OB-A: %v", err)
	}
	if err := l.submit(payoutOrg, "OB-B", thirdOrg, "AED", 300_000); err != nil {
		t.Fatalf("submit OB-B: %v", err)
	}

	l.at(baseNow + NettingWindowSeconds)
	w, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}

	if w.Postings != 1 {
		t.Fatalf("Postings = %d, want 1: the chain should collapse to a single movement", w.Postings)
	}
	if w.NetValue != 300_000 {
		t.Errorf("NetValue = %d, want 300000", w.NetValue)
	}
	if got := l.balance(payoutOrg, "AED").Balance; got != middleBefore {
		t.Errorf("intermediary balance moved from %d to %d; it netted to zero and should not have moved",
			middleBefore, got)
	}
	if got := l.balance(thirdOrg, "AED").Balance; got != 300_000 {
		t.Errorf("%s balance = %d, want 300000", thirdOrg, got)
	}
	l.assertBooksBalance()
}

// ---------------------------------------------------------------------------
// The concurrency claim, checked against real read and write sets
// ---------------------------------------------------------------------------

// Submitting an obligation must not write any balance. A balance write is what
// serialises the unnetted path, so if the netting path wrote one it would have
// bought nothing.
func TestNetting_SubmittingAnObligationWritesNoBalance(t *testing.T) {
	l := nettingLedger(t, 10_000_000)

	_, written := l.probe(senderOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SubmitObligation(c, "OB-1", payoutOrg, "AED", 100_000)
	})

	if len(written) == 0 {
		t.Fatal("submission wrote nothing at all")
	}
	for key := range written {
		switch kind := keyKind(key); kind {
		case obligationKeyPrefix, exposureKeyPrefix:
			// The obligation itself, and the submitter's own reservation.
		default:
			t.Errorf("submission wrote a %q key; the netting path must write only "+
				"its own obligation and exposure records", kind)
		}
	}
}

// Two institutions submitting at the same time must not contend. Fabric
// invalidates a transaction when another has written a key it read or wrote, so
// the test is that neither one's writes meet the other's footprint.
func TestNetting_TwoInstitutionsSubmitWithoutContending(t *testing.T) {
	l := nettingLedger(t, 10_000_000)

	// Both probes run against the same committed state and are then discarded,
	// which is the situation two concurrently endorsed proposals are in.
	senderTouched, senderWrote := l.probe(senderOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SubmitObligation(c, "OB-SENDER", payoutOrg, "AED", 100_000)
	})
	payoutTouched, payoutWrote := l.probe(payoutOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SubmitObligation(c, "OB-PAYOUT", senderOrg, "AED", 100_000)
	})

	if shared := overlap(senderTouched, payoutWrote); len(shared) > 0 {
		t.Errorf("%s writes %v, which %s also touched: the two would conflict",
			payoutOrg, shared, senderOrg)
	}
	if shared := overlap(payoutTouched, senderWrote); len(shared) > 0 {
		t.Errorf("%s writes %v, which %s also touched: the two would conflict",
			senderOrg, shared, payoutOrg)
	}
}

// Two obligations from the SAME institution do contend, on that institution's
// own exposure record, and this test pins that down rather than glossing over
// it.
//
// It is the honest limit of the design: parallelism is across institutions, not
// within one. The fix is to shard the exposure record, which is a change to one
// key's shape and nothing else — and it is not pretended to be done here.
func TestNetting_OneInstitutionSerialisesOnItsOwnExposure(t *testing.T) {
	l := nettingLedger(t, 10_000_000)

	firstTouched, _ := l.probe(senderOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SubmitObligation(c, "OB-1", payoutOrg, "AED", 100_000)
	})
	_, secondWrote := l.probe(senderOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SubmitObligation(c, "OB-2", payoutOrg, "AED", 100_000)
	})

	shared := overlap(firstTouched, secondWrote)
	if len(shared) != 1 || shared[0] != exposureKeyPrefix {
		t.Errorf("same-institution submissions share %v; expected contention on the "+
			"exposure record alone", shared)
	}
}

// ---------------------------------------------------------------------------
// Closing a window
// ---------------------------------------------------------------------------

// A window that is still accruing must not be closable. This is the property
// that makes the range query inside CloseWindow safe from phantom reads: the
// window's key range is frozen by time before anything reads it.
func TestCloseWindow_RefusesAWindowThatIsStillOpen(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	seq := l.seqNow()
	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 100_000); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if _, err := l.closeWindow(senderOrg, "AED", seq); err == nil {
		t.Fatal("closed the current window; a window must be in the past before it settles")
	}
	// A future window is even less closable.
	if _, err := l.closeWindow(senderOrg, "AED", seq+10); err == nil {
		t.Fatal("closed a future window")
	}

	l.at(baseNow + NettingWindowSeconds)
	if _, err := l.closeWindow(senderOrg, "AED", seq); err != nil {
		t.Fatalf("window should be closable once it is in the past: %v", err)
	}
}

// Closing twice must not settle twice. The window record is the idempotency
// key, exactly as the transfer record is on the two-phase commit path.
func TestCloseWindow_RejectsAReplay(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	seq := l.seqNow()
	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 100_000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	l.at(baseNow + NettingWindowSeconds)

	if _, err := l.closeWindow(senderOrg, "AED", seq); err != nil {
		t.Fatalf("first close: %v", err)
	}
	after := l.balance(payoutOrg, "AED").Balance

	if _, err := l.closeWindow(payoutOrg, "AED", seq); err == nil {
		t.Fatal("second close succeeded; a settled window must not settle again")
	}
	if got := l.balance(payoutOrg, "AED").Balance; got != after {
		t.Errorf("balance moved on the replay, from %d to %d", after, got)
	}
	l.assertBooksBalance()
}

// Any member must be able to close a window, and every member must get the same
// answer. If closing were a privileged act the corridor would have a single
// point of failure; because the netting is a pure function of frozen state,
// it does not need to be one.
func TestCloseWindow_AnyMemberClosesToTheSameResult(t *testing.T) {
	scenario := func(closer string) (Window, int64, int64) {
		t.Helper()
		l := nettingLedger(t, 10_000_000)
		seq := l.seqNow()
		if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 700_000); err != nil {
			t.Fatalf("submit OB-1: %v", err)
		}
		if err := l.submit(payoutOrg, "OB-2", senderOrg, "AED", 250_000); err != nil {
			t.Fatalf("submit OB-2: %v", err)
		}
		if err := l.submit(payoutOrg, "OB-3", thirdOrg, "AED", 400_000); err != nil {
			t.Fatalf("submit OB-3: %v", err)
		}
		l.at(baseNow + NettingWindowSeconds)
		w, err := l.closeWindow(closer, "AED", seq)
		if err != nil {
			t.Fatalf("close as %s: %v", closer, err)
		}
		l.assertBooksBalance()
		return *w, l.balance(senderOrg, "AED").Balance, l.balance(thirdOrg, "AED").Balance
	}

	byParty, partySender, partyThird := scenario(senderOrg)
	byOutsider, outsiderSender, outsiderThird := scenario(thirdOrg)

	// ClosedBy is the one field that legitimately differs.
	byParty.ClosedBy, byOutsider.ClosedBy = "", ""
	if byParty != byOutsider {
		t.Errorf("window record depends on who closed it:\n  %+v\n  %+v", byParty, byOutsider)
	}
	if partySender != outsiderSender || partyThird != outsiderThird {
		t.Errorf("balances depend on who closed the window: %d/%d vs %d/%d",
			partySender, partyThird, outsiderSender, outsiderThird)
	}
}

// An empty window must still settle, so that a quiet period does not leave a
// window that can never be closed and whose obligations could never be ruled
// out by a later reader.
func TestCloseWindow_SettlesAnEmptyWindow(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	seq := l.seqNow()
	l.at(baseNow + NettingWindowSeconds)

	w, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow on an empty window: %v", err)
	}
	if w.Obligations != 0 || w.GrossValue != 0 || w.NetValue != 0 || w.Postings != 0 {
		t.Errorf("empty window settled as %+v", w)
	}
	if w.State != WindowSettled {
		t.Errorf("State = %q, want %q", w.State, WindowSettled)
	}
	l.assertBooksBalance()
}

// The netting result must not depend on the order obligations are read in. Go
// randomises map iteration, so a fold that leaked map order would make
// endorsing peers disagree and every netted transaction would fail validation.
func TestNetting_IsDeterministicWhateverTheObligationOrder(t *testing.T) {
	obs := []Obligation{
		{ObligationID: "a", Debtor: "OrgA", Creditor: "OrgB", Ccy: "AED", Amount: 500},
		{ObligationID: "b", Debtor: "OrgB", Creditor: "OrgC", Ccy: "AED", Amount: 500},
		{ObligationID: "c", Debtor: "OrgC", Creditor: "OrgA", Ccy: "AED", Amount: 200},
		{ObligationID: "d", Debtor: "OrgA", Creditor: "OrgD", Ccy: "AED", Amount: 300},
		{ObligationID: "e", Debtor: "OrgD", Creditor: "OrgB", Ccy: "AED", Amount: 100},
	}
	want, err := pairPostings(netPositions(obs))
	if err != nil {
		t.Fatalf("pairPostings: %v", err)
	}

	// Rotating the slice exercises a different map insertion order on every
	// iteration, and the run is repeated because map ordering varies per run.
	for rotate := 0; rotate < len(obs); rotate++ {
		shuffled := append(append([]Obligation{}, obs[rotate:]...), obs[:rotate]...)
		for attempt := 0; attempt < 50; attempt++ {
			got, err := pairPostings(netPositions(shuffled))
			if err != nil {
				t.Fatalf("pairPostings (rotate %d): %v", rotate, err)
			}
			if len(got) != len(want) {
				t.Fatalf("rotate %d: %d postings, want %d", rotate, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("rotate %d posting %d = %+v, want %+v", rotate, i, got[i], want[i])
				}
			}
		}
	}
}

// pairPostings is the last thing standing between a bad fold and value being
// created, so it must refuse positions that do not sum to zero rather than
// posting a partial result.
func TestPairPostings_RefusesPositionsThatDoNotSumToZero(t *testing.T) {
	_, err := pairPostings([]netPosition{
		{Account: "OrgA", Net: -500},
		{Account: "OrgB", Net: 300},
	})
	if err == nil {
		t.Fatal("paired unbalanced positions; a short creditor side must be refused")
	}
}

// ---------------------------------------------------------------------------
// Cover and exposure
// ---------------------------------------------------------------------------

func TestSubmitObligation_RefusesACommitmentWithoutCover(t *testing.T) {
	l := nettingLedger(t, 500_000)

	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 400_000); err != nil {
		t.Fatalf("first submit should fit within cover: %v", err)
	}
	// 400,000 is already committed, so only 100,000 is left.
	if err := l.submit(senderOrg, "OB-2", payoutOrg, "AED", 200_000); err == nil {
		t.Fatal("committed more than the account holds")
	}
	if got := l.exposure(senderOrg, "AED"); got != 400_000 {
		t.Errorf("exposure = %d, want 400000: the refused submission must leave no trace", got)
	}
}

// Cover has to count commitments to windows that are past but not yet settled.
// Without that, an institution could commit its whole balance, wait for the
// window to roll, and commit it all again.
func TestSubmitObligation_CoverCountsUnsettledWindowsTogether(t *testing.T) {
	l := nettingLedger(t, 500_000)

	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 300_000); err != nil {
		t.Fatalf("submit in the first window: %v", err)
	}

	// The window rolls, but nobody has closed the old one.
	l.at(baseNow + NettingWindowSeconds)
	if err := l.submit(senderOrg, "OB-2", payoutOrg, "AED", 300_000); err == nil {
		t.Fatal("committed 600000 against a 500000 balance by waiting for the window to roll")
	}
	if err := l.submit(senderOrg, "OB-3", payoutOrg, "AED", 200_000); err != nil {
		t.Fatalf("200000 still fits within cover: %v", err)
	}
}

// The two settlement paths must not be able to spend the same money. Value
// committed to a netting window has to be invisible to the two-phase commit.
func TestExposure_BlocksTheTwoPhaseCommitFromSpendingCommittedFunds(t *testing.T) {
	l := nettingLedger(t, 1_000_000)

	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 1_000_000); err != nil {
		t.Fatalf("submit: %v", err)
	}

	expires := baseNow + 300
	if err := l.create(senderOrg, "TP-1", 1_000_000, "AED", 50_000, "AED",
		"Q-1", expires, payoutOrg, "payee-hash"); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := l.accept(payoutOrg, "TP-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	err := l.prepare(senderOrg, "TP-1")
	if err == nil {
		t.Fatal("prepared against funds already committed to a netting window")
	}
	if got := l.balance(senderOrg, "AED").Balance; got != 1_000_000 {
		t.Errorf("balance = %d, want 1000000: the refused prepare must move nothing", got)
	}
	l.assertBooksBalance()
}

// Settling a window must give the reservation back, so the funds that were
// committed and not consumed become spendable again.
func TestExposure_IsReleasedWhenTheWindowSettles(t *testing.T) {
	l := nettingLedger(t, 1_000_000)
	seq := l.seqNow()

	// Commit the whole balance, but only a tenth of it will actually move.
	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 1_000_000); err != nil {
		t.Fatalf("submit OB-1: %v", err)
	}
	if err := l.submit(payoutOrg, "OB-2", senderOrg, "AED", 900_000); err != nil {
		t.Fatalf("submit OB-2: %v", err)
	}
	if got := l.exposure(senderOrg, "AED"); got != 1_000_000 {
		t.Fatalf("exposure before close = %d, want 1000000", got)
	}

	l.at(baseNow + NettingWindowSeconds)
	if _, err := l.closeWindow(senderOrg, "AED", seq); err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}

	if got := l.exposure(senderOrg, "AED"); got != 0 {
		t.Errorf("exposure after close = %d, want 0", got)
	}
	if got := l.exposure(payoutOrg, "AED"); got != 0 {
		t.Errorf("%s exposure after close = %d, want 0", payoutOrg, got)
	}
	// 1,000,000 committed, 100,000 net paid, so 900,000 is spendable again.
	if got := l.balance(senderOrg, "AED").Balance; got != 900_000 {
		t.Fatalf("balance after close = %d, want 900000", got)
	}
	expires := baseNow + NettingWindowSeconds + 300
	if err := l.create(senderOrg, "TP-1", 900_000, "AED", 10_000, "AED",
		"Q-1", expires, payoutOrg, "payee-hash"); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := l.accept(payoutOrg, "TP-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := l.prepare(senderOrg, "TP-1"); err != nil {
		t.Fatalf("released funds should be spendable again: %v", err)
	}
	l.assertBooksBalance()
}

// ---------------------------------------------------------------------------
// Guards
// ---------------------------------------------------------------------------

func TestSubmitObligation_Guards(t *testing.T) {
	cases := []struct {
		name         string
		org          string
		obligationID string
		creditor     string
		ccy          string
		amount       uint64
	}{
		{"empty obligation ID", senderOrg, "", payoutOrg, "AED", 1000},
		{"obligation ID with a space", senderOrg, "OB 1", payoutOrg, "AED", 1000},
		{"empty creditor", senderOrg, "OB-1", "", "AED", 1000},
		{"creditor posing as issuance", senderOrg, "OB-1", "issuance:AED", "AED", 1000},
		{"creditor posing as escrow", senderOrg, "OB-1", "escrow:TP-1", "AED", 1000},
		{"creditor is the debtor", senderOrg, "OB-1", senderOrg, "AED", 1000},
		{"zero amount", senderOrg, "OB-1", payoutOrg, "AED", 0},
		{"lowercase currency", senderOrg, "OB-1", payoutOrg, "aed", 1000},
		{"currency too long", senderOrg, "OB-1", payoutOrg, "AEDX", 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := nettingLedger(t, 10_000_000)
			if err := l.submit(tc.org, tc.obligationID, tc.creditor, tc.ccy, tc.amount); err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			l.assertBooksBalance()
		})
	}
}

func TestSubmitObligation_RejectsAReplay(t *testing.T) {
	l := nettingLedger(t, 10_000_000)

	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 100_000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 100_000); err == nil {
		t.Fatal("accepted a replayed obligation ID")
	}
	if got := l.exposure(senderOrg, "AED"); got != 100_000 {
		t.Errorf("exposure = %d, want 100000: the replay must not have reserved again", got)
	}
}

// An obligation is attributed to the signer, never to an argument, so an
// institution cannot commit another's funds.
func TestSubmitObligation_AttributesTheDebtToTheSigner(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	seq := l.seqNow()

	if err := l.submit(payoutOrg, "OB-1", senderOrg, "AED", 100_000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	obs, err := l.sc.ListObligations(l.as(senderOrg), "AED", seq)
	if err != nil {
		t.Fatalf("ListObligations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("got %d obligations, want 1", len(obs))
	}
	if obs[0].Debtor != payoutOrg {
		t.Errorf("Debtor = %q, want %q", obs[0].Debtor, payoutOrg)
	}
	if got := l.exposure(payoutOrg, "AED"); got != 100_000 {
		t.Errorf("%s exposure = %d, want 100000", payoutOrg, got)
	}
	if got := l.exposure(senderOrg, "AED"); got != 0 {
		t.Errorf("%s exposure = %d, want 0: the creditor reserves nothing", senderOrg, got)
	}
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

// An operator should be able to see what a window would net to before deciding
// to close it.
func TestGetWindow_ProjectsAnOpenWindowBeforeItSettles(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	seq := l.seqNow()
	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 800_000); err != nil {
		t.Fatalf("submit OB-1: %v", err)
	}
	if err := l.submit(payoutOrg, "OB-2", senderOrg, "AED", 600_000); err != nil {
		t.Fatalf("submit OB-2: %v", err)
	}

	open := l.window("AED", seq)
	if open.State != WindowOpen {
		t.Errorf("State = %q, want %q", open.State, WindowOpen)
	}
	if open.GrossValue != 1_400_000 || open.NetValue != 200_000 || open.Postings != 1 {
		t.Errorf("projection = %+v, want gross 1400000, net 200000, 1 posting", open)
	}

	l.at(baseNow + NettingWindowSeconds)
	settled, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}
	// The projection has to agree with the settlement, or it is worse than
	// nothing: an operator would be deciding on a figure that does not hold.
	if settled.GrossValue != open.GrossValue ||
		settled.NetValue != open.NetValue ||
		settled.Postings != open.Postings ||
		settled.CompressionBps != open.CompressionBps {
		t.Errorf("settlement %+v contradicts the projection %+v", settled, open)
	}
}

func TestListWindows_ReturnsSettledWindowsInOrder(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	first := l.seqNow()

	if err := l.submit(senderOrg, "OB-1", payoutOrg, "AED", 100_000); err != nil {
		t.Fatalf("submit OB-1: %v", err)
	}
	l.at(baseNow + NettingWindowSeconds)
	second := l.seqNow()
	if err := l.submit(senderOrg, "OB-2", payoutOrg, "AED", 200_000); err != nil {
		t.Fatalf("submit OB-2: %v", err)
	}
	l.at(baseNow + 2*NettingWindowSeconds)

	if _, err := l.closeWindow(senderOrg, "AED", second); err != nil {
		t.Fatalf("close second: %v", err)
	}
	if _, err := l.closeWindow(senderOrg, "AED", first); err != nil {
		t.Fatalf("close first: %v", err)
	}

	windows, err := l.sc.ListWindows(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(windows))
	}
	// Closed out of order, but listed in order: the keys are padded so that
	// window 10 sorts after window 9 rather than before it.
	if windows[0].Seq != first || windows[1].Seq != second {
		t.Errorf("windows listed as %d, %d; want %d, %d",
			windows[0].Seq, windows[1].Seq, first, second)
	}
}

func TestCurrentWindow_FollowsTheTransactionTimestamp(t *testing.T) {
	l := newLedger(t, baseNow)
	seq, err := l.sc.CurrentWindow(l.as(senderOrg))
	if err != nil {
		t.Fatalf("CurrentWindow: %v", err)
	}
	if seq != baseNow/NettingWindowSeconds {
		t.Errorf("CurrentWindow = %d, want %d", seq, baseNow/NettingWindowSeconds)
	}

	l.at(baseNow + NettingWindowSeconds)
	rolled, err := l.sc.CurrentWindow(l.as(senderOrg))
	if err != nil {
		t.Fatalf("CurrentWindow after the roll: %v", err)
	}
	if rolled != seq+1 {
		t.Errorf("CurrentWindow = %d, want %d", rolled, seq+1)
	}
}

// netPositions promises a slice ordered by account name. Nothing downstream
// currently depends on it — pairPostings sorts again by its own key — so
// without this test the ordering could be dropped and the suite would stay
// green until some later caller quietly started relying on it on one peer and
// not another.
func TestNetPositions_AreOrderedByAccount(t *testing.T) {
	obs := []Obligation{
		{Debtor: "OrgD", Creditor: "OrgA", Ccy: "AED", Amount: 100},
		{Debtor: "OrgB", Creditor: "OrgC", Ccy: "AED", Amount: 200},
		{Debtor: "OrgA", Creditor: "OrgB", Ccy: "AED", Amount: 300},
	}
	for attempt := 0; attempt < 100; attempt++ {
		pos := netPositions(obs)
		for i := 1; i < len(pos); i++ {
			if pos[i-1].Account >= pos[i].Account {
				t.Fatalf("positions are not ordered by account: %q then %q",
					pos[i-1].Account, pos[i].Account)
			}
		}
	}
}

// The invariant that actually says netting is CORRECT, rather than merely
// balanced: every account's balance must move by exactly its net position in
// the window.
//
// Double entry already guarantees no value is created — a wrong amount applied
// to both sides of a posting keeps the books balanced while settling the wrong
// figure. This is the check that catches that, and it is the one an auditor
// would ask for.
func TestNetting_EveryAccountMovesByExactlyItsNetPosition(t *testing.T) {
	l := nettingLedger(t, 10_000_000)
	if err := l.seed(thirdOrg, "AED", 10_000_000); err != nil {
		t.Fatalf("seed %s: %v", thirdOrg, err)
	}
	seq := l.seqNow()

	// A deliberately tangled window: three institutions, flow in every
	// direction, no two amounts alike.
	obligations := []struct {
		id       string
		debtor   string
		creditor string
		amount   uint64
	}{
		{"OB-1", senderOrg, payoutOrg, 450_000},
		{"OB-2", payoutOrg, senderOrg, 120_000},
		{"OB-3", senderOrg, thirdOrg, 330_000},
		{"OB-4", thirdOrg, payoutOrg, 275_000},
		{"OB-5", payoutOrg, thirdOrg, 60_000},
		{"OB-6", thirdOrg, senderOrg, 95_000},
		{"OB-7", senderOrg, payoutOrg, 15_000},
	}

	accounts := []string{senderOrg, payoutOrg, thirdOrg}
	before := map[string]int64{}
	expected := map[string]int64{}
	for _, a := range accounts {
		before[a] = l.balance(a, "AED").Balance
	}
	for _, o := range obligations {
		if err := l.submit(o.debtor, o.id, o.creditor, "AED", o.amount); err != nil {
			t.Fatalf("submit %s: %v", o.id, err)
		}
		expected[o.debtor] -= int64(o.amount)
		expected[o.creditor] += int64(o.amount)
	}

	l.at(baseNow + NettingWindowSeconds)
	w, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}

	for _, a := range accounts {
		moved := l.balance(a, "AED").Balance - before[a]
		if moved != expected[a] {
			t.Errorf("%s moved by %d, but its net position in the window is %d",
				a, moved, expected[a])
		}
	}

	// And the settlement must be cheaper than paying each obligation on its own.
	if w.Postings >= 2*w.Obligations {
		t.Errorf("Postings = %d for %d obligations: netting bought nothing",
			w.Postings, w.Obligations)
	}
	l.assertBooksBalance()
}

// compressionBps is the number this contract exists to produce, and the
// obvious way to compute it overflows.
func TestCompressionBps(t *testing.T) {
	cases := []struct {
		name       string
		gross, net uint64
		want       int64
	}{
		{"everything cancelled", 1_000_000, 0, 10000},
		{"nothing cancelled", 1_000_000, 1_000_000, 0},
		{"the live corridor figure", 1_800_000, 100_000, 9444},
		{"floored, never rounded up", 900_000, 100_000, 8888},
		{"an empty window", 0, 0, 0},
		// net > gross cannot happen — the net is a subset of the gross — but a
		// metric that returned a negative percentage would be worse than one
		// that refuses.
		{"net somehow exceeds gross", 100, 500, 0},
		// The reason this function exists. net*10000 overflows uint64 above
		// about 1.8e15, and the naive expression reports nonsense from there on
		// — silently, on the headline number.
		{"a gross that would overflow the naive formula", math.MaxInt64, math.MaxInt64 / 2, 5000},
		{"the largest representable window", math.MaxInt64, 0, 10000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compressionBps(tc.gross, tc.net); got != tc.want {
				t.Errorf("compressionBps(%d, %d) = %d, want %d", tc.gross, tc.net, got, tc.want)
			}
		})
	}
}

// The naive expression and the exact one must agree everywhere the naive one
// is still valid, so that fixing the overflow did not change the figure for
// any window anyone will actually settle.
func TestCompressionBps_MatchesTheNaiveFormulaBelowOverflow(t *testing.T) {
	for _, gross := range []uint64{1, 7, 1000, 999_983, 1_800_000, 1 << 40} {
		for _, frac := range []uint64{0, 1, 3, 7, 10} {
			net := gross * frac / 10
			naive := int64((gross - net) * 10000 / gross)
			if got := compressionBps(gross, net); got != naive {
				t.Errorf("compressionBps(%d, %d) = %d, naive gives %d", gross, net, got, naive)
			}
		}
	}
}
