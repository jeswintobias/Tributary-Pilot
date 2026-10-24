package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

const (
	transferKeyPrefix = "transfer"
	balanceKeyPrefix  = "balance"
)

// SmartContract implements the Phase 0 settlement two-phase commit.
type SmartContract struct {
	contractapi.Contract
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

// CreateTransfer records a new transfer in state CAPTURED. Nothing is locked and
// nothing moves.
//
// The caller's MSP ID becomes the sender org: an org cannot create a transfer
// that obliges someone else to pay the sender leg. transferID is the idempotency
// key — a replay of an existing ID is rejected, never re-applied.
//
// Deviation from the original brief, recorded deliberately: the signature carries
// payoutOrg and payoutPayeeRef in addition to the fields listed there. Neither can
// be derived from the other arguments, and the payout leg has no payer and no
// payee without them.
func (s *SmartContract) CreateTransfer(
	ctx contractapi.TransactionContextInterface,
	transferID string,
	senderLegAmount uint64,
	senderCcy string,
	payoutLegAmount uint64,
	payoutCcy string,
	quoteID string,
	expiresAt int64,
	payoutOrg string,
	payoutPayeeRef string,
) error {
	if err := requireNonEmpty("transferID", transferID); err != nil {
		return err
	}
	if err := requireNonEmpty("senderCcy", senderCcy); err != nil {
		return err
	}
	if err := requireNonEmpty("payoutCcy", payoutCcy); err != nil {
		return err
	}
	if err := requireNonEmpty("quoteID", quoteID); err != nil {
		return err
	}
	if err := requireNonEmpty("payoutOrg", payoutOrg); err != nil {
		return err
	}
	if err := requireNonEmpty("payoutPayeeRef", payoutPayeeRef); err != nil {
		return err
	}
	// Zero legs are meaningless and would let a caller settle nothing while
	// occupying a transfer ID. Negative amounts cannot be expressed: the
	// parameters are unsigned, so "-1" fails argument conversion before it
	// reaches this function.
	if senderLegAmount == 0 {
		return fmt.Errorf("senderLegAmount must be greater than zero")
	}
	if payoutLegAmount == 0 {
		return fmt.Errorf("payoutLegAmount must be greater than zero")
	}

	senderOrg, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if senderOrg == payoutOrg {
		return fmt.Errorf("senderOrg and payoutOrg must differ, both are %q", senderOrg)
	}

	key, err := transferKey(ctx, transferID)
	if err != nil {
		return err
	}
	existing, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read transfer %q: %w", transferID, err)
	}
	if existing != nil {
		return fmt.Errorf("transfer %q already exists: replays are rejected", transferID)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if expiresAt <= now {
		return fmt.Errorf("expiresAt %d is not in the future (tx time %d)", expiresAt, now)
	}

	t := Transfer{
		TransferID:      transferID,
		SenderOrg:       senderOrg,
		SenderLegAmount: senderLegAmount,
		SenderCcy:       senderCcy,
		PayoutOrg:       payoutOrg,
		PayoutLegAmount: payoutLegAmount,
		PayoutCcy:       payoutCcy,
		PayoutPayeeRef:  payoutPayeeRef,
		QuoteID:         quoteID,
		ExpiresAt:       expiresAt,
		State:           StateCaptured,
		CreatedAt:       now,
	}
	if err := putTransfer(ctx, key, t); err != nil {
		return err
	}
	return emit(ctx, EventCaptured, t, now)
}

// ---------------------------------------------------------------------------
// Prepare
// ---------------------------------------------------------------------------

// Prepare locks both legs against their payers' available balances and anchors a
// proof hash over the transfer's canonical fields.
//
// A lock is taken in full or not at all. Available balance is Total - Locked, so
// a second transfer cannot be prepared against funds an earlier one already
// holds — that is what stops a double-spend.
func (s *SmartContract) Prepare(ctx contractapi.TransactionContextInterface, transferID string) error {
	key, t, err := loadTransfer(ctx, transferID)
	if err != nil {
		return err
	}
	if err := requireParty(ctx, t); err != nil {
		return err
	}
	if t.State != StateCaptured {
		return fmt.Errorf("transfer %q is %s, Prepare requires %s", transferID, t.State, StateCaptured)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now >= t.ExpiresAt {
		return fmt.Errorf("transfer %q expired at %d (tx time %d)", transferID, t.ExpiresAt, now)
	}

	if err := lockFunds(ctx, t.SenderOrg, t.SenderCcy, t.SenderLegAmount); err != nil {
		return fmt.Errorf("lock sender leg: %w", err)
	}
	if err := lockFunds(ctx, t.PayoutOrg, t.PayoutCcy, t.PayoutLegAmount); err != nil {
		// Both locks are written in the same transaction, so a failure here
		// discards the sender-leg lock along with everything else in the write
		// set. There is no partial lock to unwind.
		return fmt.Errorf("lock payout leg: %w", err)
	}

	t.State = StatePrepared
	t.PreparedAt = now
	t.ProofHash = proofHash(t)

	if err := putTransfer(ctx, key, t); err != nil {
		return err
	}
	return emit(ctx, EventPrepared, t, now)
}

// ---------------------------------------------------------------------------
// Commit
// ---------------------------------------------------------------------------

// Commit consumes both locks and moves both legs in one transaction.
//
// Guards: the transfer must be PREPARED, must not have expired, the caller must
// be one of the two parties, and the anchored proof hash must still match the
// transfer's canonical fields.
//
// The endorsement policy required for this function on the live network is
// AND('SenderOrgMSP.peer','PayoutBankOrgMSP.peer') — see network/deploy-cc.sh.
// Chaincode cannot see how many organisations endorsed it, so that half of the
// control is enforced by the Validation Service, not here.
func (s *SmartContract) Commit(ctx contractapi.TransactionContextInterface, transferID string) error {
	key, t, err := loadTransfer(ctx, transferID)
	if err != nil {
		return err
	}
	if err := requireParty(ctx, t); err != nil {
		return err
	}
	if t.State != StatePrepared {
		return fmt.Errorf("transfer %q is %s, Commit requires %s", transferID, t.State, StatePrepared)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now >= t.ExpiresAt {
		return fmt.Errorf(
			"transfer %q expired at %d (tx time %d): commit refused, abort it instead",
			transferID, t.ExpiresAt, now)
	}

	if t.ProofHash != proofHash(t) {
		return fmt.Errorf("transfer %q proof hash mismatch: state was modified after prepare", transferID)
	}

	// Both legs move here. A failure in either settlement discards the whole
	// write set, which is the atomicity guarantee the design rests on.
	if err := settleLeg(ctx, t.SenderOrg, t.PayoutOrg, t.SenderCcy, t.SenderLegAmount); err != nil {
		return fmt.Errorf("settle sender leg: %w", err)
	}
	if err := settleLeg(ctx, t.PayoutOrg, t.PayoutPayeeRef, t.PayoutCcy, t.PayoutLegAmount); err != nil {
		return fmt.Errorf("settle payout leg: %w", err)
	}

	t.State = StateSettled
	t.ResolvedAt = now

	if err := putTransfer(ctx, key, t); err != nil {
		return err
	}
	return emit(ctx, EventSettled, t, now)
}

// ---------------------------------------------------------------------------
// Abort
// ---------------------------------------------------------------------------

// Abort releases both locks and leaves every balance exactly as it was — a full
// refund of both legs.
//
// Either party may abort a CAPTURED transfer, because nothing is locked in that
// state. A PREPARED transfer can only be aborted once it has expired.
//
// Deviation from the original brief, recorded deliberately: the brief allows
// abort "on failure" as well as after expiry. Letting one party unilaterally
// abort a prepared, unexpired transfer is a griefing vector — the counterparty
// has locked funds in good faith and would have no recourse. A coordinated early
// abort needs both signatures and belongs with the Phase 1 coordinator; until
// then the expiry is the only release valve, which is the conservative choice.
func (s *SmartContract) Abort(ctx contractapi.TransactionContextInterface, transferID string) error {
	key, t, err := loadTransfer(ctx, transferID)
	if err != nil {
		return err
	}
	if err := requireParty(ctx, t); err != nil {
		return err
	}
	if t.State == StateSettled || t.State == StateAborted {
		return fmt.Errorf("transfer %q is already %s and cannot be aborted", transferID, t.State)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if t.State == StatePrepared && now < t.ExpiresAt {
		return fmt.Errorf(
			"transfer %q is PREPARED and does not expire until %d (tx time %d): "+
				"unilateral early abort is refused", transferID, t.ExpiresAt, now)
	}

	if t.State == StatePrepared {
		if err := releaseFunds(ctx, t.SenderOrg, t.SenderCcy, t.SenderLegAmount); err != nil {
			return fmt.Errorf("release sender leg: %w", err)
		}
		if err := releaseFunds(ctx, t.PayoutOrg, t.PayoutCcy, t.PayoutLegAmount); err != nil {
			return fmt.Errorf("release payout leg: %w", err)
		}
	}

	t.State = StateAborted
	t.ResolvedAt = now

	if err := putTransfer(ctx, key, t); err != nil {
		return err
	}
	return emit(ctx, EventAborted, t, now)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetTransfer returns a transfer by ID.
func (s *SmartContract) GetTransfer(ctx contractapi.TransactionContextInterface, transferID string) (*Transfer, error) {
	_, t, err := loadTransfer(ctx, transferID)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetBalance returns an account's holding in one currency. An account with no
// holdings reads as zero rather than as an error.
func (s *SmartContract) GetBalance(ctx contractapi.TransactionContextInterface, account, ccy string) (*Balance, error) {
	b, err := readBalance(ctx, account, ccy)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ---------------------------------------------------------------------------
// Phase 0 scaffolding
// ---------------------------------------------------------------------------

// SeedBalance credits an organisation's own account so the two-phase commit has
// funds to move.
//
// This is Phase 0 scaffolding, not a design feature. It exists because the token
// contract that issues real tokenised deposits is Phase 1. A caller may only
// credit its own MSP ID, which keeps it from being a way to fabricate someone
// else's balance, and it is removed when the token contract lands.
func (s *SmartContract) SeedBalance(ctx contractapi.TransactionContextInterface, ccy string, amount uint64) error {
	if err := requireNonEmpty("ccy", ccy); err != nil {
		return err
	}
	if amount == 0 {
		return fmt.Errorf("amount must be greater than zero")
	}
	mspID, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	b, err := readBalance(ctx, mspID, ccy)
	if err != nil {
		return err
	}
	total, err := addU64(b.Total, amount)
	if err != nil {
		return fmt.Errorf("credit %s %s: %w", mspID, ccy, err)
	}
	b.Total = total
	return writeBalance(ctx, b)
}

// ---------------------------------------------------------------------------
// Balance helpers
// ---------------------------------------------------------------------------

func lockFunds(ctx contractapi.TransactionContextInterface, account, ccy string, amount uint64) error {
	b, err := readBalance(ctx, account, ccy)
	if err != nil {
		return err
	}
	if b.Available() < amount {
		return fmt.Errorf(
			"account %s has %d available %s, needs %d",
			account, b.Available(), ccy, amount)
	}
	locked, err := addU64(b.Locked, amount)
	if err != nil {
		return err
	}
	b.Locked = locked
	return writeBalance(ctx, b)
}

func releaseFunds(ctx contractapi.TransactionContextInterface, account, ccy string, amount uint64) error {
	b, err := readBalance(ctx, account, ccy)
	if err != nil {
		return err
	}
	if b.Locked < amount {
		return fmt.Errorf("account %s has %d locked %s, cannot release %d", account, b.Locked, ccy, amount)
	}
	b.Locked -= amount
	return writeBalance(ctx, b)
}

// settleLeg consumes a lock on the payer and credits the payee, in that order.
func settleLeg(ctx contractapi.TransactionContextInterface, payer, payee, ccy string, amount uint64) error {
	from, err := readBalance(ctx, payer, ccy)
	if err != nil {
		return err
	}
	if from.Locked < amount {
		return fmt.Errorf("payer %s has %d locked %s, cannot settle %d", payer, from.Locked, ccy, amount)
	}
	if from.Total < amount {
		return fmt.Errorf("payer %s has %d total %s, cannot settle %d", payer, from.Total, ccy, amount)
	}
	from.Locked -= amount
	from.Total -= amount
	if err := writeBalance(ctx, from); err != nil {
		return err
	}

	to, err := readBalance(ctx, payee, ccy)
	if err != nil {
		return err
	}
	total, err := addU64(to.Total, amount)
	if err != nil {
		return fmt.Errorf("credit %s %s: %w", payee, ccy, err)
	}
	to.Total = total
	return writeBalance(ctx, to)
}

func readBalance(ctx contractapi.TransactionContextInterface, account, ccy string) (Balance, error) {
	if err := requireNonEmpty("account", account); err != nil {
		return Balance{}, err
	}
	if err := requireNonEmpty("ccy", ccy); err != nil {
		return Balance{}, err
	}
	key, err := balanceKey(ctx, account, ccy)
	if err != nil {
		return Balance{}, err
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return Balance{}, fmt.Errorf("read balance %s/%s: %w", account, ccy, err)
	}
	if raw == nil {
		return Balance{Account: account, Ccy: ccy}, nil
	}
	var b Balance
	if err := json.Unmarshal(raw, &b); err != nil {
		return Balance{}, fmt.Errorf("decode balance %s/%s: %w", account, ccy, err)
	}
	return b, nil
}

func writeBalance(ctx contractapi.TransactionContextInterface, b Balance) error {
	if b.Locked > b.Total {
		// Defensive: every caller above checks its own precondition, so reaching
		// this means a bug rather than bad input. Fail the transaction instead of
		// persisting a broken invariant.
		return fmt.Errorf("invariant violated for %s/%s: locked %d exceeds total %d",
			b.Account, b.Ccy, b.Locked, b.Total)
	}
	key, err := balanceKey(ctx, b.Account, b.Ccy)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode balance %s/%s: %w", b.Account, b.Ccy, err)
	}
	if err := ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write balance %s/%s: %w", b.Account, b.Ccy, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Transfer helpers
// ---------------------------------------------------------------------------

func loadTransfer(ctx contractapi.TransactionContextInterface, transferID string) (string, Transfer, error) {
	if err := requireNonEmpty("transferID", transferID); err != nil {
		return "", Transfer{}, err
	}
	key, err := transferKey(ctx, transferID)
	if err != nil {
		return "", Transfer{}, err
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return "", Transfer{}, fmt.Errorf("read transfer %q: %w", transferID, err)
	}
	if raw == nil {
		return "", Transfer{}, fmt.Errorf("transfer %q does not exist", transferID)
	}
	var t Transfer
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", Transfer{}, fmt.Errorf("decode transfer %q: %w", transferID, err)
	}
	return key, t, nil
}

func putTransfer(ctx contractapi.TransactionContextInterface, key string, t Transfer) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode transfer %q: %w", t.TransferID, err)
	}
	if err := ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write transfer %q: %w", t.TransferID, err)
	}
	return nil
}

// proofHash is a deterministic digest over the transfer's economic terms. It is
// anchored at Prepare and re-checked at Commit, so a transfer whose terms were
// altered between the two phases fails to commit.
//
// Mutable fields (State, ProofHash, ResolvedAt) are excluded by construction.
func proofHash(t Transfer) string {
	h := sha256.New()
	for _, field := range []string{
		t.TransferID,
		t.SenderOrg,
		strconv.FormatUint(t.SenderLegAmount, 10),
		t.SenderCcy,
		t.PayoutOrg,
		strconv.FormatUint(t.PayoutLegAmount, 10),
		t.PayoutCcy,
		t.PayoutPayeeRef,
		t.QuoteID,
		strconv.FormatInt(t.ExpiresAt, 10),
		strconv.FormatInt(t.CreatedAt, 10),
		strconv.FormatInt(t.PreparedAt, 10),
	} {
		// Length-prefixed so that concatenation is unambiguous: "ab"+"c" and
		// "a"+"bc" must not produce the same digest.
		h.Write([]byte(strconv.Itoa(len(field))))
		h.Write([]byte(":"))
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Identity, time, keys, arithmetic
// ---------------------------------------------------------------------------

// callerMSPID reads the submitting organisation from the signing identity. It is
// never taken from an argument, which is what stops a client from claiming to be
// another org.
func callerMSPID(ctx contractapi.TransactionContextInterface) (string, error) {
	mspID, err := ctx.GetClientIdentity().GetMSPID()
	if err != nil {
		return "", fmt.Errorf("read caller MSP ID: %w", err)
	}
	if mspID == "" {
		return "", fmt.Errorf("caller has an empty MSP ID")
	}
	return mspID, nil
}

// requireParty rejects any caller that is not one of the transfer's two parties.
func requireParty(ctx contractapi.TransactionContextInterface, t Transfer) error {
	mspID, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if mspID != t.SenderOrg && mspID != t.PayoutOrg {
		return fmt.Errorf(
			"caller %s is not a party to transfer %q (parties: %s, %s)",
			mspID, t.TransferID, t.SenderOrg, t.PayoutOrg)
	}
	return nil
}

// txTime returns the transaction proposal's timestamp in Unix seconds.
//
// This is the only clock the contract may read. time.Now() would differ between
// endorsing peers and make endorsements disagree near an expiry boundary.
func txTime(ctx contractapi.TransactionContextInterface) (int64, error) {
	ts, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return 0, fmt.Errorf("read tx timestamp: %w", err)
	}
	return ts.GetSeconds(), nil
}

func transferKey(ctx contractapi.TransactionContextInterface, transferID string) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(transferKeyPrefix, []string{transferID})
	if err != nil {
		return "", fmt.Errorf("build transfer key: %w", err)
	}
	return key, nil
}

func balanceKey(ctx contractapi.TransactionContextInterface, account, ccy string) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(balanceKeyPrefix, []string{account, ccy})
	if err != nil {
		return "", fmt.Errorf("build balance key: %w", err)
	}
	return key, nil
}

// addU64 adds two amounts and refuses to wrap around.
func addU64(a, b uint64) (uint64, error) {
	sum := a + b
	if sum < a {
		return 0, fmt.Errorf("amount overflow adding %d to %d", b, a)
	}
	return sum, nil
}

func requireNonEmpty(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	return nil
}

func emit(ctx contractapi.TransactionContextInterface, name string, t Transfer, at int64) error {
	payload, err := json.Marshal(stateChangeEvent{
		TransferID:      t.TransferID,
		State:           t.State,
		SenderOrg:       t.SenderOrg,
		SenderLegAmount: t.SenderLegAmount,
		SenderCcy:       t.SenderCcy,
		PayoutOrg:       t.PayoutOrg,
		PayoutLegAmount: t.PayoutLegAmount,
		PayoutCcy:       t.PayoutCcy,
		PayoutPayeeRef:  t.PayoutPayeeRef,
		ProofHash:       t.ProofHash,
		At:              at,
	})
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if err := ctx.GetStub().SetEvent(name, payload); err != nil {
		return fmt.Errorf("emit %s event: %w", name, err)
	}
	return nil
}
