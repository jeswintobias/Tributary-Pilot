package settlement

import "testing"

// unattestedLedger is funded but issues no attestations automatically, so
// these tests control compliance themselves.
func unattestedLedger(t *testing.T) *ledger {
	t.Helper()
	l := fundedLedger(t)
	l.autoAttest = false
	return l
}

// readyToPrepare gets a transfer as far as ACCEPTED without attesting anybody.
func readyToPrepare(t *testing.T, l *ledger, id string) {
	t.Helper()
	if err := l.create(senderOrg, id, senderLeg, aed, payoutLeg, inr,
		"Q-"+id, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := l.accept(payoutOrg, id); err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

func TestPrepare_RefusedWithoutAttestations(t *testing.T) {
	l := unattestedLedger(t)
	readyToPrepare(t, l, "TP-1")

	requireErrContains(t, l.prepare(senderOrg, "TP-1"), "has no KYC attestation")

	// Nothing was locked: a beneficiary who cannot be paid must not cause the
	// counterparty's funds to be held.
	if got := l.locked("TP-1", aed); got != 0 {
		t.Errorf("escrow holds %d after a refused prepare", got)
	}
	l.assertBooksBalance()
}

func TestPrepare_RefusedWithOnlyOneOfTheTwo(t *testing.T) {
	l := unattestedLedger(t)
	readyToPrepare(t, l, "TP-1")

	if err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.IssueAttestation(c, payeeRef, AttestKYC, l.stub.now+86_400, "case-1")
	}); err != nil {
		t.Fatalf("IssueAttestation: %v", err)
	}

	// Identified but not screened. Both are required.
	requireErrContains(t, l.prepare(senderOrg, "TP-1"), "has no SANCTIONS attestation")
}

func TestPrepare_SucceedsOnceBothAreLive(t *testing.T) {
	l := unattestedLedger(t)
	readyToPrepare(t, l, "TP-1")

	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	if err := l.prepare(senderOrg, "TP-1"); err != nil {
		t.Fatalf("Prepare with both attestations live: %v", err)
	}
	if got := l.locked("TP-1", aed); got != int64(senderLeg) {
		t.Errorf("escrow holds %d, want %d", got, senderLeg)
	}
}

// The test this whole file exists for. A sanctions hit arriving after funds
// are locked but before they move has to stop the payout. Checking compliance
// only at Prepare would leave a window as long as the settlement window in
// which a newly sanctioned beneficiary is still paid.
func TestCommit_RefusedWhenSanctionsLandMidSettlement(t *testing.T) {
	l := unattestedLedger(t)
	readyToPrepare(t, l, "TP-1")
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	if err := l.prepare(senderOrg, "TP-1"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// The screening provider finds something while the transfer is prepared.
	if err := l.revoke(payeeRef, AttestSanctions, "ofac-match"); err != nil {
		t.Fatalf("RevokeAttestation: %v", err)
	}

	requireErrContains(t, l.commit(payoutOrg, "TP-1"), "commit refused")

	// The funds are still in escrow, not with the payee. They are released by
	// aborting after expiry, which is the existing refund path.
	if got := l.locked("TP-1", aed); got != int64(senderLeg) {
		t.Errorf("escrow holds %d, want the legs still locked at %d", got, senderLeg)
	}
	if got := l.balance(payeeRef, inr).Balance; got != 0 {
		t.Errorf("payee received %d despite a sanctions revocation", got)
	}
	l.assertBooksBalance()
}

func TestCommit_RefusedWhenTheAttestationExpiresMidSettlement(t *testing.T) {
	l := unattestedLedger(t)
	readyToPrepare(t, l, "TP-1")

	// Attestations valid for a minute, inside the settlement window.
	for _, kind := range RequiredAttestations {
		if err := l.invoke(attestorOrg, func(c contractCtx) error {
			return l.sc.IssueAttestation(c, payeeRef, kind, l.stub.now+60, "case-1")
		}); err != nil {
			t.Fatalf("IssueAttestation %s: %v", kind, err)
		}
	}
	if err := l.prepare(senderOrg, "TP-1"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	l.at(l.stub.now + 120)
	requireErrContains(t, l.commit(payoutOrg, "TP-1"), "expired at")
	l.assertBooksBalance()
}

// An aborted transfer refunds regardless of compliance. Value returning to the
// person it came from is not a payout and must not be blocked by a sanctions
// hit — otherwise a revocation would strand the counterparty's money for ever.
func TestAbort_RefundsEvenAfterRevocation(t *testing.T) {
	l := unattestedLedger(t)
	readyToPrepare(t, l, "TP-1")
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	senderBefore := l.balance(senderOrg, aed).Balance
	if err := l.prepare(senderOrg, "TP-1"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := l.revoke(payeeRef, AttestSanctions, "ofac-match"); err != nil {
		t.Fatalf("RevokeAttestation: %v", err)
	}

	l.at(expiresAt + 1)
	if err := l.abort(senderOrg, "TP-1"); err != nil {
		t.Fatalf("Abort after revocation should still refund: %v", err)
	}
	if got := l.balance(senderOrg, aed).Balance; got != senderBefore {
		t.Errorf("sender has %d after the refund, want %d", got, senderBefore)
	}
	l.assertBooksBalance()
}

// ---------------------------------------------------------------------------
// Who may attest
// ---------------------------------------------------------------------------

func TestIssueAttestation_OnlyAnApprovedAttestorMay(t *testing.T) {
	l := unattestedLedger(t)
	for _, org := range []string{senderOrg, payoutOrg} {
		err := l.invoke(org, func(c contractCtx) error {
			return l.sc.IssueAttestation(c, payeeRef, AttestKYC, l.stub.now+86_400, "self-certified")
		})
		requireErrContains(t, err, "is not an approved attestor")
	}
}

func TestRevokeAttestation_OnlyTheIssuerMay(t *testing.T) {
	l := unattestedLedger(t)
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	// A party to the transfer cannot make its own beneficiary non-compliant,
	// nor clear one.
	err := l.invoke(senderOrg, func(c contractCtx) error {
		return l.sc.RevokeAttestation(c, payeeRef, AttestKYC, "inconvenient")
	})
	requireErrContains(t, err, "may not revoke it")
}

func TestIssueAttestation_IsAttributedToTheSigner(t *testing.T) {
	l := unattestedLedger(t)
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	att, err := l.sc.GetAttestation(l.as(senderOrg), payeeRef, AttestKYC)
	if err != nil {
		t.Fatalf("GetAttestation: %v", err)
	}
	if att.Issuer != attestorOrg {
		t.Errorf("Issuer = %q, want %q", att.Issuer, attestorOrg)
	}
}

// ---------------------------------------------------------------------------
// Validity
// ---------------------------------------------------------------------------

func TestIssueAttestation_RefusesAnUnboundedValidity(t *testing.T) {
	l := unattestedLedger(t)
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.IssueAttestation(c, payeeRef, AttestKYC,
			l.stub.now+MaxAttestationValiditySeconds+1, "forever")
	})
	requireErrContains(t, err, "a check trusted indefinitely is not a check")
}

func TestIssueAttestation_RefusesAnExpiryInThePast(t *testing.T) {
	l := unattestedLedger(t)
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.IssueAttestation(c, payeeRef, AttestKYC, l.stub.now-1, "stale")
	})
	requireErrContains(t, err, "not in the future")
}

func TestIssueAttestation_WillNotOverwriteALiveOne(t *testing.T) {
	l := unattestedLedger(t)
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.IssueAttestation(c, payeeRef, AttestKYC, l.stub.now+86_400, "again")
	})
	requireErrContains(t, err, "is live until")
}

// Re-issuing over a revocation would let an issuer quietly undo a sanctions
// hit by writing a fresh attestation on top of it.
func TestIssueAttestation_WillNotPaperOverARevocation(t *testing.T) {
	l := unattestedLedger(t)
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	if err := l.revoke(payeeRef, AttestSanctions, "ofac-match"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.IssueAttestation(c, payeeRef, AttestSanctions, l.stub.now+86_400, "second-look")
	})
	requireErrContains(t, err, "cannot be re-issued")
}

func TestIssueAttestation_RefusesAReservedSubject(t *testing.T) {
	l := unattestedLedger(t)
	for _, subject := range []string{"issuance:AED", "escrow:TP-1"} {
		err := l.invoke(attestorOrg, func(c contractCtx) error {
			return l.sc.IssueAttestation(c, subject, AttestKYC, l.stub.now+86_400, "x")
		})
		requireErrContains(t, err, "reserved account prefix")
	}
}

func TestIssueAttestation_RefusesAnUnknownKind(t *testing.T) {
	l := unattestedLedger(t)
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.IssueAttestation(c, payeeRef, "VIBES", l.stub.now+86_400, "x")
	})
	requireErrContains(t, err, "is not one of")
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

func TestCheckBeneficiary_SaysWhyAPayoutWouldBeRefused(t *testing.T) {
	l := unattestedLedger(t)

	status, err := l.sc.CheckBeneficiary(l.as(senderOrg), payeeRef)
	if err != nil {
		t.Fatalf("CheckBeneficiary: %v", err)
	}
	if status.Clear {
		t.Error("an unattested beneficiary reads as clear")
	}
	if len(status.Missing) != 2 {
		t.Errorf("Missing = %v, want both kinds", status.Missing)
	}

	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	status, err = l.sc.CheckBeneficiary(l.as(senderOrg), payeeRef)
	if err != nil {
		t.Fatalf("CheckBeneficiary: %v", err)
	}
	if !status.Clear {
		t.Errorf("an attested beneficiary reads as %q", status.Reason)
	}

	if err := l.revoke(payeeRef, AttestSanctions, "ofac-match"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	status, err = l.sc.CheckBeneficiary(l.as(senderOrg), payeeRef)
	if err != nil {
		t.Fatalf("CheckBeneficiary: %v", err)
	}
	if status.Clear {
		t.Error("a revoked beneficiary reads as clear")
	}
}

func TestAttestation_NoPersonalDataReachesTheLedger(t *testing.T) {
	l := unattestedLedger(t)
	if err := l.attest(payeeRef); err != nil {
		t.Fatalf("attest: %v", err)
	}
	all, err := l.sc.ListAttestations(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListAttestations: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d attestations, want 2", len(all))
	}
	for _, a := range all {
		// The subject is the same opaque reference the payout leg carries.
		// There is nowhere in the struct to put a name, and this asserts the
		// shape rather than trusting that nobody adds one.
		if a.SubjectHash != payeeRef {
			t.Errorf("SubjectHash = %q, want the salted reference %q", a.SubjectHash, payeeRef)
		}
	}
}
