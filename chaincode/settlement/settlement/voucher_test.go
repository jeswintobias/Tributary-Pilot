package settlement

import "testing"

// Ravi's instruction: most of it to his sister's college, the rest to his
// mother. Two recipients, one instruction.
const (
	collegeRef = "sha256:college-fees-account"
	motherRef  = "sha256:mother-upi-handle"
)

// settledTransfer runs a transfer all the way to SETTLED so a voucher has
// value that has actually arrived.
func settledTransfer(t *testing.T, l *ledger, id string) {
	t.Helper()
	if err := l.create(senderOrg, id, senderLeg, aed, payoutLeg, inr,
		"Q-"+id, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := l.accept(payoutOrg, id); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := l.prepare(senderOrg, id); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := l.commit(payoutOrg, id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// ravisSplits divides the payout leg between the college and the mother.
func ravisSplits() []VoucherSplit {
	tuition := payoutLeg / 2
	return []VoucherSplit{
		{Purpose: "TUITION", PayeeRef: collegeRef, Amount: tuition},
		{Purpose: "FAMILY", PayeeRef: motherRef, Amount: payoutLeg - tuition},
	}
}

func issueRavisVoucher(t *testing.T, l *ledger, voucherID, transferID string) {
	t.Helper()
	for _, ref := range []string{collegeRef, motherRef} {
		if err := l.attest(ref); err != nil {
			t.Fatalf("attest %s: %v", ref, err)
		}
	}
	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, voucherID, transferID, ravisSplits(), l.stub.now+3600)
	}); err != nil {
		t.Fatalf("IssueVoucher: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Binding
// ---------------------------------------------------------------------------

func TestVoucher_BindingMovesValueOutOfTheBeneficiarysBalance(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")

	if got := l.balance(payeeRef, inr).Balance; got != int64(payoutLeg) {
		t.Fatalf("beneficiary holds %d before the voucher, want %d", got, payoutLeg)
	}

	issueRavisVoucher(t, l, "V-1", "TP-1")

	// The money is no longer spendable by the beneficiary: it is bound.
	if got := l.balance(payeeRef, inr).Balance; got != 0 {
		t.Errorf("beneficiary still holds %d after binding; the value should be in the voucher", got)
	}
	if got := l.balance(VoucherAccount("V-1"), inr).Balance; got != int64(payoutLeg) {
		t.Errorf("voucher holds %d, want %d", got, payoutLeg)
	}
	l.assertBooksBalance()
}

func TestVoucher_MustAccountForTheWholePayoutLeg(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	if err := l.attest(collegeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}

	// Half the payout bound, half unaccounted for.
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", []VoucherSplit{
			{Purpose: "TUITION", PayeeRef: collegeRef, Amount: payoutLeg / 2},
		}, l.stub.now+3600)
	})
	requireErrContains(t, err, "must account for all of it")
}

func TestVoucher_OnlyThePayoutOrgMayIssue(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	for _, ref := range []string{collegeRef, motherRef} {
		if err := l.attest(ref); err != nil {
			t.Fatalf("attest: %v", err)
		}
	}
	err := l.invoke(senderOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", ravisSplits(), l.stub.now+3600)
	})
	requireErrContains(t, err, "only the payout org")
}

func TestVoucher_CannotBindValueThatHasNotArrived(t *testing.T) {
	l := fundedLedger(t)
	if err := l.create(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
		"Q-1", expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	for _, ref := range []string{collegeRef, motherRef} {
		if err := l.attest(ref); err != nil {
			t.Fatalf("attest: %v", err)
		}
	}
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", ravisSplits(), l.stub.now+3600)
	})
	requireErrContains(t, err, "a voucher binds value that has actually arrived")
}

// A voucher must not be a way to route money to somebody who could not have
// received it directly.
func TestVoucher_EveryRecipientMustBeScreened(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	// The college is screened; the mother is not.
	if err := l.attest(collegeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", ravisSplits(), l.stub.now+3600)
	})
	requireErrContains(t, err, "has no KYC attestation")

	// And nothing was bound.
	if got := l.balance(payeeRef, inr).Balance; got != int64(payoutLeg) {
		t.Errorf("beneficiary holds %d; a refused issue must move nothing", got)
	}
	l.assertBooksBalance()
}

func TestVoucher_RefusesAReservedRecipient(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", []VoucherSplit{
			{Purpose: "TUITION", PayeeRef: "voucher:V-2", Amount: payoutLeg},
		}, l.stub.now+3600)
	})
	requireErrContains(t, err, "reserved account prefix")
}

func TestVoucher_RefusesARepeatedRecipient(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	if err := l.attest(collegeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", []VoucherSplit{
			{Purpose: "TUITION", PayeeRef: collegeRef, Amount: payoutLeg / 2},
			{Purpose: "TUITION", PayeeRef: collegeRef, Amount: payoutLeg - payoutLeg/2},
		}, l.stub.now+3600)
	})
	requireErrContains(t, err, "repeats")
}

func TestVoucher_RejectsAReplay(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.IssueVoucher(c, "V-1", "TP-1", ravisSplits(), l.stub.now+3600)
	})
	requireErrContains(t, err, "already exists")
}

// ---------------------------------------------------------------------------
// Redemption
// ---------------------------------------------------------------------------

func TestVoucher_EachSplitReachesItsOwnRecipient(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")
	tuition := payoutLeg / 2

	// The college claims its fees. The family's share is untouched.
	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	}); err != nil {
		t.Fatalf("RedeemVoucher(0): %v", err)
	}
	if got := l.balance(collegeRef, inr).Balance; got != int64(tuition) {
		t.Errorf("college has %d, want %d", got, tuition)
	}
	if got := l.balance(motherRef, inr).Balance; got != 0 {
		t.Errorf("the family's share moved early: %d", got)
	}

	v, err := l.sc.GetVoucher(l.as(payoutOrg), "V-1")
	if err != nil {
		t.Fatalf("GetVoucher: %v", err)
	}
	if v.State != VoucherIssued {
		t.Errorf("State = %q after one of two splits, want %q", v.State, VoucherIssued)
	}

	// And then the family's.
	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 1)
	}); err != nil {
		t.Fatalf("RedeemVoucher(1): %v", err)
	}
	if got := l.balance(motherRef, inr).Balance; got != int64(payoutLeg-tuition) {
		t.Errorf("mother has %d, want %d", got, payoutLeg-tuition)
	}
	if got := l.balance(VoucherAccount("V-1"), inr).Balance; got != 0 {
		t.Errorf("voucher still holds %d after both splits", got)
	}

	v, err = l.sc.GetVoucher(l.as(payoutOrg), "V-1")
	if err != nil {
		t.Fatalf("GetVoucher: %v", err)
	}
	if v.State != VoucherRedeemed {
		t.Errorf("State = %q, want %q", v.State, VoucherRedeemed)
	}
	l.assertBooksBalance()
}

func TestVoucher_ASplitCannotBeRedeemedTwice(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	}); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	})
	requireErrContains(t, err, "already redeemed")
	l.assertBooksBalance()
}

// A voucher can outlive a sanctions decision, and the money moves at
// redemption rather than at issue.
func TestVoucher_RedemptionIsRefusedAfterARevocation(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	if err := l.revoke(collegeRef, AttestSanctions, "ofac-match"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	})
	requireErrContains(t, err, "redemption refused")

	if got := l.balance(collegeRef, inr).Balance; got != 0 {
		t.Errorf("the college received %d despite the revocation", got)
	}
	l.assertBooksBalance()
}

func TestVoucher_CannotBeRedeemedAfterExpiry(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	l.at(l.stub.now + 7200)
	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	})
	requireErrContains(t, err, "refund it instead")
}

func TestVoucher_OnlyTheIssuerMayRedeem(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	err := l.invoke(senderOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	})
	requireErrContains(t, err, "only the issuing org")
}

func TestVoucher_RefusesASplitIndexOutOfRange(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 7)
	})
	requireErrContains(t, err, "is out of range")
}

// ---------------------------------------------------------------------------
// Refund
// ---------------------------------------------------------------------------

func TestVoucher_UnredeemedValueGoesBackOnExpiry(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")
	tuition := payoutLeg / 2

	// The college takes its fees; the family never claims theirs.
	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RedeemVoucher(c, "V-1", 0)
	}); err != nil {
		t.Fatalf("RedeemVoucher: %v", err)
	}

	l.at(l.stub.now + 7200)
	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RefundVoucher(c, "V-1")
	}); err != nil {
		t.Fatalf("RefundVoucher: %v", err)
	}

	// The college keeps what it took; the remainder returns to the
	// beneficiary, who is the person the money arrived for.
	if got := l.balance(collegeRef, inr).Balance; got != int64(tuition) {
		t.Errorf("college has %d, want %d", got, tuition)
	}
	if got := l.balance(payeeRef, inr).Balance; got != int64(payoutLeg-tuition) {
		t.Errorf("beneficiary got back %d, want %d", got, payoutLeg-tuition)
	}
	if got := l.balance(VoucherAccount("V-1"), inr).Balance; got != 0 {
		t.Errorf("voucher still holds %d after the refund", got)
	}
	l.assertBooksBalance()
}

// Refunding early would defeat the constraint: the recipient simply has not
// claimed yet.
func TestVoucher_CannotBeRefundedBeforeExpiry(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")

	err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RefundVoucher(c, "V-1")
	})
	requireErrContains(t, err, "does not expire until")
}

func TestVoucher_OnlyAPartyMayRefund(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")
	l.at(l.stub.now + 7200)

	err := l.invoke(thirdOrg, func(c contractCtx) error {
		return l.sc.RefundVoucher(c, "V-1")
	})
	requireErrContains(t, err, "is not a party")
}

func TestVoucher_RefundIsNotRepeatable(t *testing.T) {
	l := fundedLedger(t)
	settledTransfer(t, l, "TP-1")
	issueRavisVoucher(t, l, "V-1", "TP-1")
	l.at(l.stub.now + 7200)

	if err := l.invoke(payoutOrg, func(c contractCtx) error {
		return l.sc.RefundVoucher(c, "V-1")
	}); err != nil {
		t.Fatalf("RefundVoucher: %v", err)
	}
	err := l.invoke(senderOrg, func(c contractCtx) error {
		return l.sc.RefundVoucher(c, "V-1")
	})
	requireErrContains(t, err, "has nothing to refund")
	l.assertBooksBalance()
}
