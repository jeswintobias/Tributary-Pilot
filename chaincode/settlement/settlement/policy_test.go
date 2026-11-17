package settlement

import "testing"

// The policy admin set is real MSP IDs, not the harness's logical names for
// the two trading parties. On the live network SenderOrg is Org1MSP and
// PayoutBankOrg is Org2MSP, so these are those two organisations; here they
// are named for the role they play in maker-checker rather than for the side
// of the corridor they sit on.
const (
	policyMaker   = "Org1MSP"
	policyChecker = "Org2MSP"
)

// setPolicy puts a policy into effect through the real maker-checker path:
// proposed by one organisation, approved by another.
func setPolicy(t *testing.T, l *ledger, p Policy) {
	t.Helper()
	var proposalID string
	if err := l.invoke(policyMaker, func(c contractCtx) error {
		var err error
		proposalID, err = l.sc.ProposePolicy(c, p)
		return err
	}); err != nil {
		t.Fatalf("ProposePolicy: %v", err)
	}
	if err := l.invoke(policyChecker, func(c contractCtx) error {
		return l.sc.ApprovePolicy(c, proposalID)
	}); err != nil {
		t.Fatalf("ApprovePolicy: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Maker-checker
// ---------------------------------------------------------------------------

// The control this whole file exists for. One compromised key must not be able
// to raise a limit.
func TestPolicy_ProposerCannotApproveItsOwnChange(t *testing.T) {
	l := fundedLedger(t)
	var proposalID string
	if err := l.invoke(policyMaker, func(c contractCtx) error {
		var err error
		proposalID, err = l.sc.ProposePolicy(c, Policy{
			MaxSingleTransfer: map[string]uint64{aed: 1},
		})
		return err
	}); err != nil {
		t.Fatalf("ProposePolicy: %v", err)
	}

	err := l.invoke(policyMaker, func(c contractCtx) error {
		return l.sc.ApprovePolicy(c, proposalID)
	})
	requireErrContains(t, err, "may not also approve it")

	// And nothing took effect.
	p, err := l.sc.GetPolicy(l.as(senderOrg))
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if p.Version != 0 {
		t.Errorf("policy version = %d, want 0: an unapproved proposal must not be active", p.Version)
	}
}

func TestPolicy_ADifferentOrgCanApprove(t *testing.T) {
	l := fundedLedger(t)
	setPolicy(t, l, Policy{MaxSingleTransfer: map[string]uint64{aed: 500_000}})

	p, err := l.sc.GetPolicy(l.as(senderOrg))
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if p.Version != 1 {
		t.Errorf("Version = %d, want 1", p.Version)
	}
	if p.ProposedBy != policyMaker || p.ApprovedBy != policyChecker {
		t.Errorf("proposed by %q approved by %q; want %q and %q",
			p.ProposedBy, p.ApprovedBy, policyMaker, policyChecker)
	}
	if p.MaxSingleTransfer[aed] != 500_000 {
		t.Errorf("MaxSingleTransfer[AED] = %d, want 500000", p.MaxSingleTransfer[aed])
	}
}

func TestPolicy_OnlyAnAdminOrgMayPropose(t *testing.T) {
	l := fundedLedger(t)
	err := l.invoke("OutsiderMSP", func(c contractCtx) error {
		_, err := l.sc.ProposePolicy(c, Policy{})
		return err
	})
	requireErrContains(t, err, "may not propose policy")
}

func TestPolicy_ApprovingAnUnknownProposalIsRefused(t *testing.T) {
	l := fundedLedger(t)
	err := l.invoke(policyChecker, func(c contractCtx) error {
		return l.sc.ApprovePolicy(c, "deadbeef")
	})
	requireErrContains(t, err, "no policy proposal")
}

// The proposal is keyed by the hash of its contents, so an approver approves
// exactly what was proposed and not whatever is sitting under that ID later.
func TestPolicy_ProposalIDIsTheHashOfItsContents(t *testing.T) {
	a := Policy{MaxSingleTransfer: map[string]uint64{aed: 100}}
	b := Policy{MaxSingleTransfer: map[string]uint64{aed: 101}}
	if policyHash(a) == policyHash(b) {
		t.Fatal("two different policies hash the same, so an approval could be redirected")
	}

	// Fields set at activation are not part of what is agreed to.
	withMeta := a
	withMeta.Version = 9
	withMeta.ActivedAt = 123
	withMeta.ApprovedBy = "SomeoneMSP"
	if policyHash(a) != policyHash(withMeta) {
		t.Error("activation metadata changes the hash, so a proposal could never be approved")
	}
}

// Go randomises map iteration, so hashing the limit maps in iteration order
// would give each endorsing peer a different proposal ID and nothing would
// ever be approvable.
func TestPolicy_HashIsDeterministicAcrossMapOrder(t *testing.T) {
	p := Policy{
		MaxSingleTransfer: map[string]uint64{"AED": 1, "INR": 2, "USD": 3, "EUR": 4, "GBP": 5},
		MaxObligation:     map[string]uint64{"AED": 9, "INR": 8, "USD": 7},
		ApprovedMembers:   []string{"Org2MSP", "Org1MSP", "Org3MSP"},
	}
	first := policyHash(p)
	for i := 0; i < 200; i++ {
		if policyHash(p) != first {
			t.Fatal("policyHash is not deterministic; peers would compute different proposal IDs")
		}
	}
}

func TestPolicy_MemberOrderDoesNotChangeTheHash(t *testing.T) {
	a := Policy{ApprovedMembers: []string{"Org1MSP", "Org2MSP"}}
	b := Policy{ApprovedMembers: []string{"Org2MSP", "Org1MSP"}}
	if policyHash(a) != policyHash(b) {
		t.Error("the same member set in a different order hashes differently")
	}
}

// ---------------------------------------------------------------------------
// Enforcement
// ---------------------------------------------------------------------------

func TestPolicy_CapsASingleTransfer(t *testing.T) {
	l := fundedLedger(t)
	setPolicy(t, l, Policy{MaxSingleTransfer: map[string]uint64{aed: senderLeg}})

	if err := l.create(senderOrg, "TP-AT-LIMIT", senderLeg, aed, payoutLeg, inr,
		"Q-1", expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("a transfer exactly at the limit should be allowed: %v", err)
	}
	requireErrContains(t,
		l.create(senderOrg, "TP-OVER", senderLeg+1, aed, payoutLeg, inr,
			"Q-2", expiresAt, payoutOrg, payeeRef),
		"exceeds the policy limit")
}

func TestPolicy_CapIsPerCurrency(t *testing.T) {
	l := fundedLedger(t)
	setPolicy(t, l, Policy{MaxSingleTransfer: map[string]uint64{"USD": 1}})

	// The cap is on USD; an AED transfer is untouched by it.
	if err := l.create(senderOrg, "TP-AED", senderLeg, aed, payoutLeg, inr,
		"Q-1", expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("a currency with no cap should be unaffected: %v", err)
	}
}

func TestPolicy_RestrictsWhoMayBeAParty(t *testing.T) {
	l := fundedLedger(t)
	setPolicy(t, l, Policy{ApprovedMembers: []string{senderOrg}})

	requireErrContains(t,
		l.create(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			"Q-1", expiresAt, payoutOrg, payeeRef),
		"is not an approved member")
}

func TestPolicy_EmptyMemberListMeansEveryone(t *testing.T) {
	l := fundedLedger(t)
	setPolicy(t, l, Policy{MaxSingleTransfer: map[string]uint64{aed: 10_000_000}})

	if err := l.create(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
		"Q-1", expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("an empty member list must not exclude anybody: %v", err)
	}
}

func TestPolicy_FloorsTheSettlementWindow(t *testing.T) {
	l := fundedLedger(t)
	setPolicy(t, l, Policy{MinSettlementWindowSeconds: 600})

	requireErrContains(t,
		l.create(senderOrg, "TP-RUSH", senderLeg, aed, payoutLeg, inr,
			"Q-1", l.stub.now+60, payoutOrg, payeeRef),
		"below the policy minimum")
}

func TestPolicy_CapsANettingObligation(t *testing.T) {
	l := fundedLedger(t)
	if err := l.seed(senderOrg, aed, 10_000_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	setPolicy(t, l, Policy{MaxObligation: map[string]uint64{aed: 100_000}})

	if err := l.submit(senderOrg, "OB-OK", payoutOrg, aed, 100_000); err != nil {
		t.Fatalf("an obligation at the limit should be allowed: %v", err)
	}
	requireErrContains(t,
		l.submit(senderOrg, "OB-OVER", payoutOrg, aed, 100_001),
		"exceeds the policy limit")
}

// A channel with no policy must behave exactly as it did before policy
// existed. A mechanism that bricks the system when unset is one nobody deploys.
func TestPolicy_UnsetImposesNothing(t *testing.T) {
	l := fundedLedger(t)
	p, err := l.sc.GetPolicy(l.as(senderOrg))
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if p.Version != 0 || len(p.MaxSingleTransfer) != 0 || len(p.ApprovedMembers) != 0 {
		t.Errorf("an unset policy reads as %+v, want an empty one", p)
	}
	if err := l.create(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
		"Q-1", expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("no policy must mean no limits: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestPolicy_RefusesAnUnsatisfiableWindow(t *testing.T) {
	l := fundedLedger(t)
	err := l.invoke(policyMaker, func(c contractCtx) error {
		_, err := l.sc.ProposePolicy(c, Policy{
			MinSettlementWindowSeconds: MaxSettlementWindowSeconds + 1,
		})
		return err
	})
	requireErrContains(t, err, "no transfer could ever satisfy it")
}

func TestPolicy_RefusesAMalformedCurrency(t *testing.T) {
	l := fundedLedger(t)
	err := l.invoke(policyMaker, func(c contractCtx) error {
		_, err := l.sc.ProposePolicy(c, Policy{
			MaxSingleTransfer: map[string]uint64{"rupees": 1},
		})
		return err
	})
	requireErrContains(t, err, "must be three letters")
}

func TestPolicy_ApprovalConsumesTheProposal(t *testing.T) {
	l := fundedLedger(t)
	policy := Policy{MaxSingleTransfer: map[string]uint64{aed: 1_000_000}}

	var proposalID string
	if err := l.invoke(policyMaker, func(c contractCtx) error {
		var err error
		proposalID, err = l.sc.ProposePolicy(c, policy)
		return err
	}); err != nil {
		t.Fatalf("ProposePolicy: %v", err)
	}
	if err := l.invoke(policyChecker, func(c contractCtx) error {
		return l.sc.ApprovePolicy(c, proposalID)
	}); err != nil {
		t.Fatalf("ApprovePolicy: %v", err)
	}

	// Approving again must not bump the version a second time.
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.ApprovePolicy(c, proposalID)
	})
	requireErrContains(t, err, "no policy proposal")

	pending, err := l.sc.ListPolicyProposals(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListPolicyProposals: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d proposals still pending after approval", len(pending))
	}
}
