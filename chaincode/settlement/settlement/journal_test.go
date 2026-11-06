package settlement

import "testing"

// The journal's promise is that an unbalanced entry cannot be expressed, so
// conservation holds by construction rather than by assertion. These tests
// check that promise directly, and check the two counter-accounts that make it
// true even when money enters or is locked.

func TestJournal_IssuingBalancesAgainstALiabilityAccount(t *testing.T) {
	l := newLedger(t, t0)
	if err := l.seed(senderOrg, aed, 1_000_000); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if got := l.balance(senderOrg, aed).Balance; got != 1_000_000 {
		t.Fatalf("holder has %d, want 1000000", got)
	}
	// Money did not appear: it was issued against a liability that now carries
	// the exact mirror image.
	if got := l.balance(IssuanceAccount(aed), aed).Balance; got != -1_000_000 {
		t.Fatalf("issuance shows %d, want -1000000", got)
	}
	l.assertBooksBalance()
}

func TestJournal_EveryStateChangeLeavesTheBooksBalanced(t *testing.T) {
	// Walk a transfer through every transition and check conservation after
	// each one, in both the settle and the abort direction.
	for _, ending := range []string{"settle", "abort"} {
		t.Run(ending, func(t *testing.T) {
			l := fundedLedger(t)
			l.assertBooksBalance()

			mustCreate(t, l)
			l.assertBooksBalance()

			mustAccept(t, l)
			l.assertBooksBalance()

			mustPrepare(t, l)
			l.assertBooksBalance()
			// Locked value is in escrow, not annotated on the payer.
			if got := l.locked(transferID, aed); got != int64(senderLeg) {
				t.Fatalf("escrow holds %d AED, want %d", got, senderLeg)
			}
			if got := l.balance(senderOrg, aed).Balance; got != int64(1_000_000-senderLeg) {
				t.Fatalf("payer has %d, want %d", got, 1_000_000-senderLeg)
			}

			if ending == "settle" {
				if err := l.commit(payoutOrg, transferID); err != nil {
					t.Fatalf("Commit: %v", err)
				}
				if got := l.balance(payeeRef, inr).Balance; got != int64(payoutLeg) {
					t.Fatalf("payee has %d, want %d", got, payoutLeg)
				}
			} else {
				l.at(expiresAt + 1)
				if err := l.abort(payoutOrg, transferID); err != nil {
					t.Fatalf("Abort: %v", err)
				}
				// Refunded in full: the payer is whole again.
				if got := l.balance(senderOrg, aed).Balance; got != 1_000_000 {
					t.Fatalf("payer has %d after abort, want 1000000", got)
				}
			}

			// Either way the escrow is empty and the books balance.
			if got := l.locked(transferID, aed); got != 0 {
				t.Fatalf("escrow still holds %d AED", got)
			}
			if got := l.locked(transferID, inr); got != 0 {
				t.Fatalf("escrow still holds %d INR", got)
			}
			l.assertBooksBalance()
		})
	}
}

// The read-your-writes defect, re-tested through the journal: an account that
// is touched twice in one transaction must see its own first posting.
func TestJournal_TwoPostingsToOneAccountInOneTransaction(t *testing.T) {
	l := newLedger(t, t0)
	if err := l.seed(senderOrg, inr, 10_000_000); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if err := l.seed(payoutOrg, inr, 10_000_000); err != nil {
		t.Fatalf("seed payout: %v", err)
	}

	// Both legs in one currency, so Commit credits and debits the payout org
	// inside a single transaction.
	const sLeg, pLeg uint64 = 4_000_000, 1_000_000
	if err := l.create(senderOrg, "ONETX", sLeg, inr, pLeg, inr,
		quoteID, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := l.accept(payoutOrg, "ONETX"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := l.prepare(senderOrg, "ONETX"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := l.commit(payoutOrg, "ONETX"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	want := int64(10_000_000) + int64(sLeg) - int64(pLeg)
	if got := l.balance(payoutOrg, inr).Balance; got != want {
		t.Fatalf("payout bank = %d, want %d — a posting was lost", got, want)
	}
	l.assertBooksBalance()
}

func TestJournal_RefusesUnpostableEntries(t *testing.T) {
	l := fundedLedger(t)
	j := newJournal(l.as(senderOrg))

	if err := j.post(KindSettle, "r", senderOrg, payoutOrg, aed, 0); err == nil {
		t.Fatal("a zero-amount entry was accepted")
	}
	if err := j.post(KindSettle, "r", senderOrg, senderOrg, aed, 1); err == nil {
		t.Fatal("an entry debiting and crediting one account was accepted")
	}
	// Overdrawing a real account is refused; the issuance account is the only
	// one allowed to go negative, which is what makes it the issuer.
	if err := j.post(KindSettle, "r", senderOrg, payoutOrg, aed, 99_999_999); err == nil {
		t.Fatal("an overdraft was accepted")
	}
}

// A payee must not be able to name one of the journal's own counter-accounts:
// posing as issuance would mean being credited from nothing, and posing as
// another transfer's escrow would mean draining its locked funds.
func TestCreateTransfer_RejectsReservedAccountNames(t *testing.T) {
	l := fundedLedger(t)
	for _, ref := range []string{
		IssuanceAccount(inr),
		EscrowAccount("some-other-transfer"),
	} {
		if err := l.create(senderOrg, "RSV", senderLeg, aed, payoutLeg, inr,
			quoteID, expiresAt, payoutOrg, ref); err == nil {
			t.Fatalf("payee %q was accepted", ref)
		} else {
			requireErrContains(t, err, "reserved account prefix")
		}
	}
}
