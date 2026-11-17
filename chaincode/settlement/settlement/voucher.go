package settlement

import (
	"encoding/json"
	"fmt"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
)

// Purpose-bound payout vouchers.
//
// THE PROBLEM
//
// Ravi sends money home and wants part of it to reach his sister's college and
// the rest to his mother. Today the ledger settles the payout leg to one
// beneficiary reference and the story ends there: what happens next is a
// promise somebody made off-chain, and the whole point of settling on a shared
// ledger was to stop relying on those.
//
// A voucher binds the money to its purpose. Value moves out of the
// beneficiary's balance into the voucher at issue, and can only leave it to
// the recipients the voucher names, for the purposes it names, before the date
// it names. What is not redeemed goes back.
//
// WHY THIS IS NOT JUST A TRANSFER WITH EXTRA STEPS
//
// A split could be modelled as several transfers. It is not, for two reasons.
// The sender's instruction is one instruction and has to succeed or fail as
// one — paying the college but not the mother is not a partial success. And
// the deadline is a property of the instruction rather than of each payment:
// the refund has to be of whatever is left over, which needs the pieces to
// share an identity.
//
// WHAT IS STILL OFF-CHAIN
//
// That the recipient really is the college. The voucher names a salted hash,
// and the attestation says somebody screened it. Nothing here can tell whether
// the institution behind that hash is the one Ravi meant, and no amount of
// ledger design changes that.

const voucherKeyPrefix = "voucher"

// Voucher states.
const (
	VoucherIssued   = "ISSUED"
	VoucherRedeemed = "REDEEMED" // every split taken
	VoucherRefunded = "REFUNDED" // the remainder returned after expiry
)

// MaxVoucherSplits bounds one instruction. Unbounded splits would let a caller
// write an arbitrarily large object to every peer's disk in one transaction,
// and an instruction with a hundred recipients is not a remittance.
const MaxVoucherSplits = 10

// VoucherSplit is one purpose-bound share.
type VoucherSplit struct {
	Purpose  string `json:"purpose"`  // TUITION, FAMILY, RENT — free text, recorded not enforced
	PayeeRef string `json:"payeeRef"` // salted hash of the recipient, never PII
	Amount   uint64 `json:"amount"`

	Redeemed   bool   `json:"redeemed"`
	RedeemedAt int64  `json:"redeemedAt"`
	RedeemedBy string `json:"redeemedBy"`
}

// Voucher is a settled payout bound to how it may be spent.
type Voucher struct {
	VoucherID  string `json:"voucherID"`
	TransferID string `json:"transferID"`
	Issuer     string `json:"issuer"` // the payout org
	Ccy        string `json:"ccy"`

	TotalAmount    uint64 `json:"totalAmount"`
	RedeemedAmount uint64 `json:"redeemedAmount"`

	Splits    []VoucherSplit `json:"splits"`
	ExpiresAt int64          `json:"expiresAt"`
	State     string         `json:"state"`

	IssuedAt   int64 `json:"issuedAt"`
	ResolvedAt int64 `json:"resolvedAt"`
}

// VoucherAccount holds a voucher's bound value while it is live.
func VoucherAccount(voucherID string) string { return voucherAccountPrefix + voucherID }

// ---------------------------------------------------------------------------
// Issue
// ---------------------------------------------------------------------------

// IssueVoucher binds a settled payout to a set of purposes and recipients.
//
// Only the payout organisation may issue, because it is the one holding the
// beneficiary's funds. The splits must account for the whole payout leg
// exactly: a voucher that bound only part of it would leave the remainder
// unaccounted for in a record that claims to say where the money went.
func (s *SmartContract) IssueVoucher(
	ctx contractapi.TransactionContextInterface,
	voucherID string,
	transferID string,
	splits []VoucherSplit,
	expiresAt int64,
) error {
	if err := requireID("voucherID", voucherID, maxIDLen); err != nil {
		return err
	}
	if len(splits) == 0 {
		return fmt.Errorf("a voucher must have at least one split")
	}
	if len(splits) > MaxVoucherSplits {
		return fmt.Errorf("a voucher may have at most %d splits, got %d", MaxVoucherSplits, len(splits))
	}

	_, t, err := loadTransfer(ctx, transferID)
	if err != nil {
		return err
	}
	caller, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if caller != t.PayoutOrg {
		return fmt.Errorf(
			"only the payout org %s may issue a voucher against transfer %q, caller is %s",
			t.PayoutOrg, transferID, caller)
	}
	if t.State != StateSettled {
		return fmt.Errorf(
			"transfer %q is %s; a voucher binds value that has actually arrived",
			transferID, t.State)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if expiresAt <= now {
		return fmt.Errorf("expiresAt %d is not in the future (tx time %d)", expiresAt, now)
	}

	key, err := ctx.GetStub().CreateCompositeKey(voucherKeyPrefix, []string{voucherID})
	if err != nil {
		return fmt.Errorf("build voucher key: %w", err)
	}
	existing, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read voucher %q: %w", voucherID, err)
	}
	if existing != nil {
		return fmt.Errorf("voucher %q already exists: replays are rejected", voucherID)
	}

	total := uint64(0)
	seen := map[string]bool{}
	for i, split := range splits {
		if err := requireID(fmt.Sprintf("splits[%d].purpose", i), split.Purpose, maxIDLen); err != nil {
			return err
		}
		if err := requireID(fmt.Sprintf("splits[%d].payeeRef", i), split.PayeeRef, maxPayeeRef); err != nil {
			return err
		}
		if isReservedAccount(split.PayeeRef) {
			return fmt.Errorf("splits[%d].payeeRef uses a reserved account prefix", i)
		}
		if split.Amount == 0 {
			return fmt.Errorf("splits[%d] moves nothing", i)
		}
		// One recipient twice in one instruction is almost always a mistake,
		// and it makes the redemption record ambiguous.
		pair := split.Purpose + "\x00" + split.PayeeRef
		if seen[pair] {
			return fmt.Errorf("splits[%d] repeats %s for %s", i, split.Purpose, split.PayeeRef)
		}
		seen[pair] = true

		// Every recipient must be screened, exactly as the original
		// beneficiary was. A voucher must not be a way to route value to
		// somebody who could not have received it directly.
		if err := requireAttestations(ctx, split.PayeeRef, now); err != nil {
			return fmt.Errorf("splits[%d] recipient: %w", i, err)
		}

		total, err = addU64(total, split.Amount)
		if err != nil {
			return fmt.Errorf("voucher total: %w", err)
		}
		splits[i].Redeemed = false
	}

	if total != t.PayoutLegAmount {
		return fmt.Errorf(
			"splits total %d but the payout leg is %d %s: a voucher must account for all of it",
			total, t.PayoutLegAmount, t.PayoutCcy)
	}

	// The value leaves the beneficiary's balance and becomes bound. After this
	// it can only reach the named recipients or come back on expiry.
	j := newJournal(ctx)
	if err := j.post(KindVoucherBind, voucherID, t.PayoutPayeeRef,
		VoucherAccount(voucherID), t.PayoutCcy, total); err != nil {
		return fmt.Errorf("bind voucher: %w", err)
	}

	v := Voucher{
		VoucherID: voucherID, TransferID: transferID, Issuer: caller,
		Ccy: t.PayoutCcy, TotalAmount: total, Splits: splits,
		ExpiresAt: expiresAt, State: VoucherIssued, IssuedAt: now,
	}
	if err := putVoucher(ctx, key, v); err != nil {
		return err
	}
	return emitVoucher(ctx, EventVoucherIssued, v)
}

// ---------------------------------------------------------------------------
// Redeem
// ---------------------------------------------------------------------------

// RedeemVoucher pays one split to its named recipient.
//
// Splits are redeemed individually because they are presented individually:
// the college claims its fees when the term starts, the family draws theirs
// when they need it. Requiring them together would make the slowest recipient
// hold up everyone else.
func (s *SmartContract) RedeemVoucher(
	ctx contractapi.TransactionContextInterface,
	voucherID string,
	splitIndex int,
) error {
	key, v, err := loadVoucher(ctx, voucherID)
	if err != nil {
		return err
	}
	caller, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if caller != v.Issuer {
		return fmt.Errorf(
			"only the issuing org %s may redeem voucher %q, caller is %s",
			v.Issuer, voucherID, caller)
	}
	if v.State != VoucherIssued {
		return fmt.Errorf("voucher %q is %s and cannot be redeemed", voucherID, v.State)
	}
	if splitIndex < 0 || splitIndex >= len(v.Splits) {
		return fmt.Errorf("voucher %q has %d splits; %d is out of range", voucherID, len(v.Splits), splitIndex)
	}
	split := v.Splits[splitIndex]
	if split.Redeemed {
		return fmt.Errorf(
			"split %d of voucher %q was already redeemed at %d by %s",
			splitIndex, voucherID, split.RedeemedAt, split.RedeemedBy)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now >= v.ExpiresAt {
		return fmt.Errorf(
			"voucher %q expired at %d (tx time %d): refund it instead",
			voucherID, v.ExpiresAt, now)
	}

	// Screened again at redemption, not only at issue. A voucher can outlive a
	// sanctions decision, and the money moves now.
	if err := requireAttestations(ctx, split.PayeeRef, now); err != nil {
		return fmt.Errorf("redemption refused: %w", err)
	}

	j := newJournal(ctx)
	if err := j.post(KindVoucherRedeem, voucherID, VoucherAccount(voucherID),
		split.PayeeRef, v.Ccy, split.Amount); err != nil {
		return fmt.Errorf("redeem split %d: %w", splitIndex, err)
	}

	v.Splits[splitIndex].Redeemed = true
	v.Splits[splitIndex].RedeemedAt = now
	v.Splits[splitIndex].RedeemedBy = caller
	v.RedeemedAmount, err = addU64(v.RedeemedAmount, split.Amount)
	if err != nil {
		return fmt.Errorf("voucher %q redeemed total: %w", voucherID, err)
	}
	if v.RedeemedAmount == v.TotalAmount {
		v.State = VoucherRedeemed
		v.ResolvedAt = now
	}

	if err := putVoucher(ctx, key, v); err != nil {
		return err
	}
	return emitVoucher(ctx, EventVoucherRedeemed, v)
}

// ---------------------------------------------------------------------------
// Refund
// ---------------------------------------------------------------------------

// RefundVoucher returns whatever was never redeemed, once the voucher has
// expired.
//
// Back to the beneficiary the payout settled to, not to the sender. The money
// arrived and is theirs; what the voucher constrained was the purpose, and an
// expired constraint should release the funds rather than claw them back
// across a border.
//
// Either party to the original transfer may call it, because leaving expired
// value bound depends on somebody bothering, and both have reason to.
func (s *SmartContract) RefundVoucher(
	ctx contractapi.TransactionContextInterface,
	voucherID string,
) error {
	key, v, err := loadVoucher(ctx, voucherID)
	if err != nil {
		return err
	}
	_, t, err := loadTransfer(ctx, v.TransferID)
	if err != nil {
		return err
	}
	if err := requireParty(ctx, t); err != nil {
		return err
	}
	if v.State != VoucherIssued {
		return fmt.Errorf("voucher %q is %s and has nothing to refund", voucherID, v.State)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now < v.ExpiresAt {
		return fmt.Errorf(
			"voucher %q does not expire until %d (tx time %d): "+
				"refunding early would defeat the purpose it was bound to",
			voucherID, v.ExpiresAt, now)
	}

	remainder := v.TotalAmount - v.RedeemedAmount
	if remainder > 0 {
		j := newJournal(ctx)
		if err := j.post(KindVoucherRefund, voucherID, VoucherAccount(voucherID),
			t.PayoutPayeeRef, v.Ccy, remainder); err != nil {
			return fmt.Errorf("refund voucher: %w", err)
		}
	}

	v.State = VoucherRefunded
	v.ResolvedAt = now
	if err := putVoucher(ctx, key, v); err != nil {
		return err
	}
	return emitVoucher(ctx, EventVoucherRefunded, v)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetVoucher returns one voucher.
func (s *SmartContract) GetVoucher(ctx contractapi.TransactionContextInterface, voucherID string) (*Voucher, error) {
	_, v, err := loadVoucher(ctx, voucherID)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVouchers returns every voucher, in key order. Query-only.
func (s *SmartContract) ListVouchers(ctx contractapi.TransactionContextInterface) ([]*Voucher, error) {
	out := []*Voucher{}
	err := forEachInRange(ctx, voucherKeyPrefix, []string{}, func(kv *queryresult.KV) error {
		var v Voucher
		if err := json.Unmarshal(kv.GetValue(), &v); err != nil {
			return fmt.Errorf("decode voucher at %q: %w", kv.GetKey(), err)
		}
		out = append(out, &v)
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

func loadVoucher(ctx contractapi.TransactionContextInterface, voucherID string) (string, Voucher, error) {
	if err := requireNonEmpty("voucherID", voucherID); err != nil {
		return "", Voucher{}, err
	}
	key, err := ctx.GetStub().CreateCompositeKey(voucherKeyPrefix, []string{voucherID})
	if err != nil {
		return "", Voucher{}, fmt.Errorf("build voucher key: %w", err)
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return "", Voucher{}, fmt.Errorf("read voucher %q: %w", voucherID, err)
	}
	if raw == nil {
		return "", Voucher{}, fmt.Errorf("voucher %q does not exist", voucherID)
	}
	var v Voucher
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", Voucher{}, fmt.Errorf("decode voucher %q: %w", voucherID, err)
	}
	return key, v, nil
}

func putVoucher(ctx contractapi.TransactionContextInterface, key string, v Voucher) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode voucher %q: %w", v.VoucherID, err)
	}
	if err := ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write voucher %q: %w", v.VoucherID, err)
	}
	return nil
}

func emitVoucher(ctx contractapi.TransactionContextInterface, name string, v Voucher) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if err := ctx.GetStub().SetEvent(name, payload); err != nil {
		return fmt.Errorf("emit %s event: %w", name, err)
	}
	return nil
}
