package settlement

import (
	"encoding/json"
	"fmt"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
)

// KYC and sanctions attestations.
//
// WHAT THIS CHANGES
//
// Until now the only thing the contract knew about a beneficiary was an opaque
// reference. Whether that person had been identified, or screened against a
// sanctions list, was somebody else's business happening somewhere else, and
// the ledger settled regardless. The README said so: "not compliance built
// in".
//
// Now a payout leg cannot move to a beneficiary without a live attestation
// that they were identified and screened, and the check happens in the same
// transaction as the movement. There is no window between "compliance said
// yes" and "the money left" for the answer to change.
//
// STILL NO PII
//
// An attestation is about a salted hash, never a person. The subject is the
// same opaque reference the payout leg already names, computed off-chain. The
// ledger therefore records that *some* beneficiary was screened by *some*
// issuer until *some* date, which is what an auditor needs, and nothing that
// identifies anybody, which is what the regulation requires.
//
// The honest limit of that: a salted hash is only as private as its salt. If
// the salt leaks, the hash of a known name can be recomputed and matched, so
// the ledger reveals which beneficiaries transacted. The salt lives in the
// compliance service and never on the chain. This is a real exposure and it is
// not solved by hashing alone.
//
// WHAT AN ATTESTATION IS NOT
//
// It is not evidence of compliance. It is a claim by a named issuer, with a
// date on it, that they performed a check. The ledger holds them accountable
// for having said so; it cannot know whether they looked.

const attestationKeyPrefix = "attest"

// Attestation kinds. Both are required before a payout leg may move.
const (
	// AttestKYC says the issuer identified the beneficiary.
	AttestKYC = "KYC"
	// AttestSanctions says the issuer screened the beneficiary and found
	// nothing. A hit is expressed by revoking this, not by issuing a negative.
	AttestSanctions = "SANCTIONS"
)

// RequiredAttestations must all be live for a beneficiary before the payout
// leg may settle.
var RequiredAttestations = []string{AttestKYC, AttestSanctions}

// AttestorMSPs may issue and revoke.
//
// A constant, with the same caveat as OracleMSPs: production wants this in the
// channel configuration so that adding an attestor is a config update rather
// than a chaincode upgrade. PlatformOrg is the NPCI platform node and the
// natural issuer here; it runs no peer of its own and does not need one,
// because it signs with its own identity and another organisation's peer
// endorses.
var AttestorMSPs = map[string]bool{
	"Org3MSP": true,
}

// MaxAttestationValiditySeconds bounds how long an attestation may be issued
// for.
//
// A check performed once and trusted for ever is not a check. One year matches
// the periodic-review cycle most KYC regimes expect, and the bound exists so
// that an issuer cannot — by mistake or otherwise — write an attestation that
// outlives any reasonable review.
const MaxAttestationValiditySeconds int64 = 365 * 24 * 60 * 60

// Attestation is one issuer's dated claim about one subject.
type Attestation struct {
	SubjectHash string `json:"subjectHash"` // salted hash, never PII
	Kind        string `json:"kind"`
	Issuer      string `json:"issuer"` // MSP ID, taken from the signature
	IssuedAt    int64  `json:"issuedAt"`
	ExpiresAt   int64  `json:"expiresAt"`

	// EvidenceRef points at whatever the issuer kept off-chain: a case file, a
	// screening run. Opaque here on purpose.
	EvidenceRef string `json:"evidenceRef"`

	Revoked      bool   `json:"revoked"`
	RevokedAt    int64  `json:"revokedAt"`
	RevokedBy    string `json:"revokedBy"`
	RevokeReason string `json:"revokeReason"`
}

// Live reports whether this attestation counts at the given time.
func (a Attestation) Live(now int64) bool {
	return !a.Revoked && now < a.ExpiresAt
}

// ---------------------------------------------------------------------------
// Issue and revoke
// ---------------------------------------------------------------------------

// IssueAttestation records that the caller checked a subject.
//
// The issuer is the signing MSP, never an argument: an organisation cannot
// issue an attestation in somebody else's name, and the ledger's record of who
// vouched for a beneficiary is therefore as strong as that organisation's key.
func (s *SmartContract) IssueAttestation(
	ctx contractapi.TransactionContextInterface,
	subjectHash string,
	kind string,
	expiresAt int64,
	evidenceRef string,
) error {
	if err := requireID("subjectHash", subjectHash, maxPayeeRef); err != nil {
		return err
	}
	if err := requireAttestationKind(kind); err != nil {
		return err
	}
	if evidenceRef != "" {
		if err := requireID("evidenceRef", evidenceRef, maxPayeeRef); err != nil {
			return err
		}
	}
	// The journal's counter-accounts are not people and cannot be attested.
	if isReservedAccount(subjectHash) {
		return fmt.Errorf("subjectHash %q uses a reserved account prefix", subjectHash)
	}

	issuer, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if !AttestorMSPs[issuer] {
		return fmt.Errorf("%s is not an approved attestor", issuer)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if expiresAt <= now {
		return fmt.Errorf("expiresAt %d is not in the future (tx time %d)", expiresAt, now)
	}
	if expiresAt > now+MaxAttestationValiditySeconds {
		return fmt.Errorf(
			"expiresAt %d is more than %d seconds ahead (tx time %d): "+
				"a check trusted indefinitely is not a check",
			expiresAt, MaxAttestationValiditySeconds, now)
	}

	key, err := attestationKey(ctx, subjectHash, kind)
	if err != nil {
		return err
	}
	// Re-issuing is how renewal works, but only over an attestation that is no
	// longer live. Overwriting a live one would let an issuer silently extend
	// an attestation that a revocation had already killed.
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read attestation %s/%s: %w", subjectHash, kind, err)
	}
	if raw != nil {
		var existing Attestation
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("decode attestation %s/%s: %w", subjectHash, kind, err)
		}
		if existing.Revoked {
			return fmt.Errorf(
				"attestation %s/%s was revoked by %s at %d (%s) and cannot be re-issued: "+
					"issue it under a new subject reference after a fresh review",
				subjectHash, kind, existing.RevokedBy, existing.RevokedAt, existing.RevokeReason)
		}
		if existing.Live(now) {
			return fmt.Errorf(
				"attestation %s/%s is live until %d; renew it after it expires",
				subjectHash, kind, existing.ExpiresAt)
		}
	}

	att := Attestation{
		SubjectHash: subjectHash, Kind: kind, Issuer: issuer,
		IssuedAt: now, ExpiresAt: expiresAt, EvidenceRef: evidenceRef,
	}
	encoded, err := json.Marshal(att)
	if err != nil {
		return fmt.Errorf("encode attestation: %w", err)
	}
	if err := ctx.GetStub().PutState(key, encoded); err != nil {
		return fmt.Errorf("write attestation: %w", err)
	}
	return emitAttestation(ctx, EventAttestationIssued, att)
}

// RevokeAttestation withdraws a claim. This is how a sanctions hit is
// expressed.
//
// Only the issuer may revoke, which keeps one attestor from quietly
// invalidating another's work. The record is kept, with the reason, because
// the fact that somebody was once attested and then was not is exactly what an
// auditor needs to see.
func (s *SmartContract) RevokeAttestation(
	ctx contractapi.TransactionContextInterface,
	subjectHash string,
	kind string,
	reason string,
) error {
	if err := requireAttestationKind(kind); err != nil {
		return err
	}
	if reason != "" {
		if err := requireID("reason", reason, maxPayeeRef); err != nil {
			return err
		}
	}
	caller, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	key, att, err := loadAttestation(ctx, subjectHash, kind)
	if err != nil {
		return err
	}
	if att.Issuer != caller {
		return fmt.Errorf(
			"attestation %s/%s was issued by %s; %s may not revoke it",
			subjectHash, kind, att.Issuer, caller)
	}
	if att.Revoked {
		return fmt.Errorf("attestation %s/%s is already revoked", subjectHash, kind)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	att.Revoked = true
	att.RevokedAt = now
	att.RevokedBy = caller
	att.RevokeReason = reason

	encoded, err := json.Marshal(att)
	if err != nil {
		return fmt.Errorf("encode attestation: %w", err)
	}
	if err := ctx.GetStub().PutState(key, encoded); err != nil {
		return fmt.Errorf("write attestation: %w", err)
	}
	return emitAttestation(ctx, EventAttestationRevoked, att)
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// requireAttestations refuses unless every required attestation is live for
// this subject at this instant.
//
// Called from Prepare and again from Commit. Both, not either:
//
//   - at Prepare, so a counterparty's funds are never locked against a
//     beneficiary who cannot be paid
//   - at Commit, because that is when value actually moves, and a sanctions
//     hit landing between the two phases has to stop it
//
// Checking only at Prepare would leave a window as long as the settlement
// window in which a newly sanctioned beneficiary still gets paid.
func requireAttestations(ctx contractapi.TransactionContextInterface, subjectHash string, now int64) error {
	for _, kind := range RequiredAttestations {
		_, att, err := loadAttestation(ctx, subjectHash, kind)
		if err != nil {
			return fmt.Errorf("beneficiary %s has no %s attestation: %w", subjectHash, kind, err)
		}
		if att.Revoked {
			return fmt.Errorf(
				"beneficiary %s had its %s attestation revoked by %s at %d (%s)",
				subjectHash, kind, att.RevokedBy, att.RevokedAt, att.RevokeReason)
		}
		if now >= att.ExpiresAt {
			return fmt.Errorf(
				"beneficiary %s has a %s attestation that expired at %d (tx time %d)",
				subjectHash, kind, att.ExpiresAt, now)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetAttestation returns one attestation.
func (s *SmartContract) GetAttestation(
	ctx contractapi.TransactionContextInterface,
	subjectHash, kind string,
) (*Attestation, error) {
	if err := requireAttestationKind(kind); err != nil {
		return nil, err
	}
	_, att, err := loadAttestation(ctx, subjectHash, kind)
	if err != nil {
		return nil, err
	}
	return &att, nil
}

// AttestationStatus reports whether a beneficiary may be paid, and why not.
//
// A query, so a client can tell a user "this payout will be refused because
// the sanctions check expired" before they try, instead of after.
type AttestationStatus struct {
	SubjectHash string        `json:"subjectHash"`
	Clear       bool          `json:"clear"`
	Reason      string        `json:"reason"`
	Have        []Attestation `json:"have"`
	Missing     []string      `json:"missing"`
}

// CheckBeneficiary reports attestation status without changing anything.
func (s *SmartContract) CheckBeneficiary(
	ctx contractapi.TransactionContextInterface,
	subjectHash string,
) (*AttestationStatus, error) {
	if err := requireNonEmpty("subjectHash", subjectHash); err != nil {
		return nil, err
	}
	now, err := txTime(ctx)
	if err != nil {
		return nil, err
	}
	status := &AttestationStatus{SubjectHash: subjectHash, Have: []Attestation{}, Missing: []string{}}
	for _, kind := range RequiredAttestations {
		_, att, err := loadAttestation(ctx, subjectHash, kind)
		if err != nil {
			status.Missing = append(status.Missing, kind)
			continue
		}
		status.Have = append(status.Have, att)
		if !att.Live(now) {
			status.Missing = append(status.Missing, kind)
		}
	}
	if len(status.Missing) == 0 {
		status.Clear = true
		status.Reason = "all required attestations are live"
	} else {
		status.Reason = fmt.Sprintf("not payable: %v missing, expired or revoked", status.Missing)
	}
	return status, nil
}

// ListAttestations returns every attestation, in key order. Query-only.
func (s *SmartContract) ListAttestations(ctx contractapi.TransactionContextInterface) ([]*Attestation, error) {
	out := []*Attestation{}
	err := forEachInRange(ctx, attestationKeyPrefix, []string{}, func(kv *queryresult.KV) error {
		var a Attestation
		if err := json.Unmarshal(kv.GetValue(), &a); err != nil {
			return fmt.Errorf("decode attestation at %q: %w", kv.GetKey(), err)
		}
		out = append(out, &a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func requireAttestationKind(kind string) error {
	for _, k := range RequiredAttestations {
		if kind == k {
			return nil
		}
	}
	return fmt.Errorf("kind %q is not one of %v", kind, RequiredAttestations)
}

func attestationKey(ctx contractapi.TransactionContextInterface, subjectHash, kind string) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(attestationKeyPrefix, []string{subjectHash, kind})
	if err != nil {
		return "", fmt.Errorf("build attestation key: %w", err)
	}
	return key, nil
}

func loadAttestation(ctx contractapi.TransactionContextInterface, subjectHash, kind string) (string, Attestation, error) {
	if err := requireNonEmpty("subjectHash", subjectHash); err != nil {
		return "", Attestation{}, err
	}
	key, err := attestationKey(ctx, subjectHash, kind)
	if err != nil {
		return "", Attestation{}, err
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return "", Attestation{}, fmt.Errorf("read attestation %s/%s: %w", subjectHash, kind, err)
	}
	if raw == nil {
		return "", Attestation{}, fmt.Errorf("no %s attestation for %s", kind, subjectHash)
	}
	var a Attestation
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", Attestation{}, fmt.Errorf("decode attestation %s/%s: %w", subjectHash, kind, err)
	}
	return key, a, nil
}

func emitAttestation(ctx contractapi.TransactionContextInterface, name string, a Attestation) error {
	payload, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if err := ctx.GetStub().SetEvent(name, payload); err != nil {
		return fmt.Errorf("emit %s event: %w", name, err)
	}
	return nil
}
