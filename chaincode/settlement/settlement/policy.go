package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
)

// Operating limits, and maker-checker over changing them.
//
// WHAT THIS IS FOR
//
// Everything else in this contract answers "is this transfer valid". A policy
// answers a different question: "is this transfer within what we agreed to do
// at all". A transfer can be perfectly well formed, correctly priced and
// settled to a screened beneficiary, and still be one nobody intended the
// system to make — ten times the usual size, or to an organisation that was
// never meant to be a counterparty.
//
// It matters most for the Phase 1 agent. An automated process proposing
// movements needs a bound it cannot argue with, and that bound has to live
// where the agent cannot reach it. Here is where.
//
// MAKER-CHECKER IS THE POINT
//
// Changing a limit is more dangerous than any single transfer, because it
// changes every transfer after it. So a policy change takes two organisations:
// one proposes and a DIFFERENT one approves. One compromised key raises no
// limits.
//
// The proposal is keyed by the hash of its own contents, which means the
// approver approves exactly what was proposed. Approving by ID while the
// contents could change underneath would be worse than no second signature at
// all — it would record an approval that nobody gave.

const (
	policyKeyPrefix   = "policy"
	proposalKeyPrefix = "policyprop"
	activePolicyKey   = "active"
)

// PolicyAdminMSPs may propose and approve policy. Same caveat as elsewhere:
// production wants this in the channel configuration rather than compiled in.
var PolicyAdminMSPs = map[string]bool{
	"Org1MSP": true,
	"Org2MSP": true,
	"Org3MSP": true,
}

// MinPolicyApprovers is how many distinct organisations a change needs. Two,
// and they must differ — see ApprovePolicy.
const MinPolicyApprovers = 2

// Policy is the operating envelope.
//
// A zero value means "no limit", so a channel with no policy behaves exactly
// as it did before one existed. That is deliberate: a policy mechanism that
// bricks the system when unset is a policy mechanism nobody dares deploy.
type Policy struct {
	// MaxSingleTransfer caps one transfer's sender leg, per currency, in minor
	// units. Absent or zero means no cap for that currency.
	MaxSingleTransfer map[string]uint64 `json:"maxSingleTransfer"`

	// MaxObligation caps one netting obligation, per currency.
	MaxObligation map[string]uint64 `json:"maxObligation"`

	// ApprovedMembers, when non-empty, is the only set of organisations that
	// may be party to a transfer. Empty means everyone on the channel.
	ApprovedMembers []string `json:"approvedMembers"`

	// MinSettlementWindowSeconds floors how long a transfer may be given to
	// settle. The contract already caps it; this stops a transfer being
	// created with so short a window that the counterparty cannot act.
	MinSettlementWindowSeconds int64 `json:"minSettlementWindowSeconds"`

	Version    int64  `json:"version"`
	ActivedAt  int64  `json:"activatedAt"`
	ProposedBy string `json:"proposedBy"`
	ApprovedBy string `json:"approvedBy"`
}

// PolicyProposal is a change waiting for its second signature.
type PolicyProposal struct {
	ProposalID string `json:"proposalID"` // hash of the proposed policy
	Policy     Policy `json:"policy"`
	ProposedBy string `json:"proposedBy"`
	ProposedAt int64  `json:"proposedAt"`
}

// ---------------------------------------------------------------------------
// Proposing and approving
// ---------------------------------------------------------------------------

// ProposePolicy records a change for another organisation to approve.
//
// Nothing takes effect here. The proposer's own approval is not counted, which
// is what makes this maker-checker rather than a two-step form.
func (s *SmartContract) ProposePolicy(
	ctx contractapi.TransactionContextInterface,
	policy Policy,
) (string, error) {
	mspID, err := callerMSPID(ctx)
	if err != nil {
		return "", err
	}
	if !PolicyAdminMSPs[mspID] {
		return "", fmt.Errorf("%s may not propose policy", mspID)
	}
	if err := validatePolicy(policy); err != nil {
		return "", err
	}
	now, err := txTime(ctx)
	if err != nil {
		return "", err
	}

	// The ID is the hash of the contents, so an approver cannot be made to
	// approve something other than what was proposed.
	proposalID := policyHash(policy)
	key, err := ctx.GetStub().CreateCompositeKey(proposalKeyPrefix, []string{proposalID})
	if err != nil {
		return "", fmt.Errorf("build proposal key: %w", err)
	}
	existing, err := ctx.GetStub().GetState(key)
	if err != nil {
		return "", fmt.Errorf("read proposal %q: %w", proposalID, err)
	}
	if existing != nil {
		return "", fmt.Errorf("an identical policy is already proposed as %q", proposalID)
	}

	raw, err := json.Marshal(PolicyProposal{
		ProposalID: proposalID, Policy: policy,
		ProposedBy: mspID, ProposedAt: now,
	})
	if err != nil {
		return "", fmt.Errorf("encode proposal: %w", err)
	}
	if err := ctx.GetStub().PutState(key, raw); err != nil {
		return "", fmt.Errorf("write proposal: %w", err)
	}
	return proposalID, nil
}

// ApprovePolicy activates a proposed change, if the caller is not the proposer.
//
// The refusal to let a proposer approve their own change is the whole control.
// Without it a single compromised key could raise every limit in the system,
// and the second signature would be decoration.
func (s *SmartContract) ApprovePolicy(
	ctx contractapi.TransactionContextInterface,
	proposalID string,
) error {
	if err := requireID("proposalID", proposalID, 128); err != nil {
		return err
	}
	mspID, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if !PolicyAdminMSPs[mspID] {
		return fmt.Errorf("%s may not approve policy", mspID)
	}

	key, err := ctx.GetStub().CreateCompositeKey(proposalKeyPrefix, []string{proposalID})
	if err != nil {
		return fmt.Errorf("build proposal key: %w", err)
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read proposal %q: %w", proposalID, err)
	}
	if raw == nil {
		return fmt.Errorf("no policy proposal %q", proposalID)
	}
	var proposal PolicyProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("decode proposal %q: %w", proposalID, err)
	}
	if proposal.ProposedBy == mspID {
		return fmt.Errorf(
			"%s proposed this policy and may not also approve it: "+
				"a change needs %d distinct organisations",
			mspID, MinPolicyApprovers)
	}
	// The contents must still hash to the ID they were filed under. This
	// cannot fail through the public API, and is checked anyway because the
	// whole value of hashing the proposal is that nobody has to trust it did
	// not change.
	if policyHash(proposal.Policy) != proposalID {
		return fmt.Errorf("proposal %q does not match its own hash", proposalID)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	current, err := activePolicy(ctx)
	if err != nil {
		return err
	}

	next := proposal.Policy
	next.Version = current.Version + 1
	next.ActivedAt = now
	next.ProposedBy = proposal.ProposedBy
	next.ApprovedBy = mspID

	activeKey, err := ctx.GetStub().CreateCompositeKey(policyKeyPrefix, []string{activePolicyKey})
	if err != nil {
		return fmt.Errorf("build policy key: %w", err)
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode policy: %w", err)
	}
	if err := ctx.GetStub().PutState(activeKey, encoded); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	// The proposal is consumed, so one approval activates one change.
	if err := ctx.GetStub().DelState(key); err != nil {
		return fmt.Errorf("clear proposal: %w", err)
	}
	return emitPolicy(ctx, next)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetPolicy returns the active policy. An unset policy reads as an empty one,
// which imposes no limits.
func (s *SmartContract) GetPolicy(ctx contractapi.TransactionContextInterface) (*Policy, error) {
	p, err := activePolicy(ctx)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPolicyProposals returns changes awaiting a second signature.
func (s *SmartContract) ListPolicyProposals(ctx contractapi.TransactionContextInterface) ([]*PolicyProposal, error) {
	out := []*PolicyProposal{}
	err := forEachInRange(ctx, proposalKeyPrefix, []string{}, func(kv *queryresult.KV) error {
		var p PolicyProposal
		if err := json.Unmarshal(kv.GetValue(), &p); err != nil {
			return fmt.Errorf("decode proposal at %q: %w", kv.GetKey(), err)
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Enforcement
// ---------------------------------------------------------------------------

// checkTransferAgainstPolicy is called from CreateTransfer.
//
// At creation rather than at settlement, because a transfer outside policy
// should never exist to be accepted or prepared. Refusing it later would mean
// a counterparty had already agreed to something the channel does not permit.
func checkTransferAgainstPolicy(
	ctx contractapi.TransactionContextInterface,
	senderOrg, payoutOrg, senderCcy string,
	senderAmount uint64,
	expiresAt, now int64,
) error {
	p, err := activePolicy(ctx)
	if err != nil {
		return err
	}
	if cap, ok := p.MaxSingleTransfer[senderCcy]; ok && cap > 0 && senderAmount > cap {
		return fmt.Errorf(
			"sender leg %d %s exceeds the policy limit of %d (policy version %d)",
			senderAmount, senderCcy, cap, p.Version)
	}
	if err := p.requireApproved(senderOrg); err != nil {
		return err
	}
	if err := p.requireApproved(payoutOrg); err != nil {
		return err
	}
	if p.MinSettlementWindowSeconds > 0 && expiresAt-now < p.MinSettlementWindowSeconds {
		return fmt.Errorf(
			"settlement window of %ds is below the policy minimum of %ds: "+
				"the counterparty would have no time to act",
			expiresAt-now, p.MinSettlementWindowSeconds)
	}
	return nil
}

// checkObligationAgainstPolicy is called from SubmitObligation.
func checkObligationAgainstPolicy(
	ctx contractapi.TransactionContextInterface,
	debtor, creditor, ccy string,
	amount uint64,
) error {
	p, err := activePolicy(ctx)
	if err != nil {
		return err
	}
	if cap, ok := p.MaxObligation[ccy]; ok && cap > 0 && amount > cap {
		return fmt.Errorf(
			"obligation of %d %s exceeds the policy limit of %d (policy version %d)",
			amount, ccy, cap, p.Version)
	}
	if err := p.requireApproved(debtor); err != nil {
		return err
	}
	return p.requireApproved(creditor)
}

func (p Policy) requireApproved(mspID string) error {
	if len(p.ApprovedMembers) == 0 {
		return nil
	}
	for _, m := range p.ApprovedMembers {
		if m == mspID {
			return nil
		}
	}
	return fmt.Errorf(
		"%s is not an approved member under policy version %d", mspID, p.Version)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func activePolicy(ctx contractapi.TransactionContextInterface) (Policy, error) {
	key, err := ctx.GetStub().CreateCompositeKey(policyKeyPrefix, []string{activePolicyKey})
	if err != nil {
		return Policy{}, fmt.Errorf("build policy key: %w", err)
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return Policy{}, fmt.Errorf("read policy: %w", err)
	}
	if raw == nil {
		// No policy set. An empty one imposes nothing.
		return Policy{}, nil
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, fmt.Errorf("decode policy: %w", err)
	}
	return p, nil
}

func validatePolicy(p Policy) error {
	for ccy := range p.MaxSingleTransfer {
		if err := requireCcy("maxSingleTransfer currency", ccy); err != nil {
			return err
		}
	}
	for ccy := range p.MaxObligation {
		if err := requireCcy("maxObligation currency", ccy); err != nil {
			return err
		}
	}
	for _, m := range p.ApprovedMembers {
		if err := requireID("approvedMembers entry", m, maxIDLen); err != nil {
			return err
		}
	}
	if p.MinSettlementWindowSeconds < 0 {
		return fmt.Errorf("minSettlementWindowSeconds must not be negative")
	}
	if p.MinSettlementWindowSeconds > MaxSettlementWindowSeconds {
		return fmt.Errorf(
			"minSettlementWindowSeconds %d exceeds the contract's maximum window of %d, "+
				"so no transfer could ever satisfy it",
			p.MinSettlementWindowSeconds, MaxSettlementWindowSeconds)
	}
	return nil
}

// policyHash is a deterministic digest over a policy's contents.
//
// Maps are sorted before hashing. Go randomises map iteration order, so
// hashing them as they come would give a different ID on each endorsing peer
// and no proposal would ever be approvable.
//
// Fields set at activation — version, timestamps, who proposed and approved —
// are excluded, because they are not part of what is being agreed to.
func policyHash(p Policy) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte(":"))
		h.Write([]byte(s))
	}
	write("tributary-policy-v1")

	writeCaps := func(label string, caps map[string]uint64) {
		write(label)
		keys := make([]string, 0, len(caps))
		for k := range caps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			write(k)
			write(strconv.FormatUint(caps[k], 10))
		}
	}
	writeCaps("maxSingleTransfer", p.MaxSingleTransfer)
	writeCaps("maxObligation", p.MaxObligation)

	write("approvedMembers")
	members := append([]string(nil), p.ApprovedMembers...)
	sort.Strings(members)
	for _, m := range members {
		write(m)
	}
	write("minSettlementWindowSeconds")
	write(strconv.FormatInt(p.MinSettlementWindowSeconds, 10))

	return hex.EncodeToString(h.Sum(nil))
}

func emitPolicy(ctx contractapi.TransactionContextInterface, p Policy) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", EventPolicyActivated, err)
	}
	if err := ctx.GetStub().SetEvent(EventPolicyActivated, payload); err != nil {
		return fmt.Errorf("emit %s event: %w", EventPolicyActivated, err)
	}
	return nil
}
