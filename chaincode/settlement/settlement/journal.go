package settlement

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// Double-entry journal.
//
// Balances are never written directly. Every movement of value appends one
// immutable Entry naming the account that loses value and the account that
// gains it, and the per-account totals are a materialised view folded from
// those entries.
//
// The property this buys: an entry carries a single amount applied to both
// sides, so an unbalanced entry cannot be expressed. Conservation is not an
// assertion a test remembers to make — it is a consequence of the data model.
// Summed across every account, every currency is always exactly zero.
//
// Money that enters the system is not an exception. Issuing tokens debits
// `issuance:<CCY>`, a liability account that goes negative by exactly the
// amount outstanding, so "how much have we issued" is a balance you can read
// rather than a number you track separately.
//
// Locks are not a flag on a balance either. Preparing a transfer moves value
// into `escrow:<transferID>`, a real account. Funds under lock have genuinely
// left the payer's balance, so "available" needs no arithmetic: it is the
// balance. Commit moves escrow to the payee; abort moves it home.
//
// Extraction note: this is a package inside the settlement chaincode rather
// than the separate `token` chaincode the roadmap describes. Three things make
// that move non-trivial and none of them are solved by moving the code —
// a called chaincode sees the original client's identity rather than the
// caller's, a transaction must satisfy both chaincodes' endorsement policies,
// and a caller still cannot read back what the callee wrote in the same
// transaction. The interface below is the one a separate contract would
// expose, so the move is mechanical when those are addressed.

const (
	entryKeyPrefix   = "entry"
	accountKeyPrefix = "acct"

	// Counter-accounts. The prefixes are reserved: a caller-supplied account
	// name may not begin with either, or it could pose as the issuer.
	issuanceAccountPrefix = "issuance:"
	escrowAccountPrefix   = "escrow:"
)

// Entry kinds, recorded so the journal reads as a history rather than a list
// of numbers.
const (
	KindIssue   = "ISSUE"   // issuance:<ccy> -> holder
	KindLock    = "LOCK"    // holder -> escrow:<transferID>
	KindRelease = "RELEASE" // escrow:<transferID> -> holder (abort)
	KindSettle  = "SETTLE"  // escrow:<transferID> -> payee (commit)
)

// Entry is one immutable movement of value. It is never updated or deleted.
//
// Debit is the account value leaves, Credit the account it arrives at, and
// Amount applies to both — which is what makes the entry balanced by
// construction.
type Entry struct {
	EntryID string `json:"entryID"` // "<txID>:<index>", unique and ordered
	TxID    string `json:"txID"`
	Ref     string `json:"ref"`  // the transfer this belongs to
	Kind    string `json:"kind"` // one of the Kind constants
	Debit   string `json:"debit"`
	Credit  string `json:"credit"`
	Ccy     string `json:"ccy"`
	Amount  uint64 `json:"amount"` // integer minor units, as everywhere else
	At      int64  `json:"at"`
}

// AccountBalance is the materialised fold of one account's entries in one
// currency.
//
// Debits and Credits are kept rather than a single signed total so that the
// record stays in the project's unsigned money type, and so the two sides can
// be reconciled against the journal independently. Balance is derived.
type AccountBalance struct {
	Account string `json:"account"`
	Ccy     string `json:"ccy"`
	Debits  uint64 `json:"debits"`
	Credits uint64 `json:"credits"`
	Balance int64  `json:"balance"` // Credits - Debits; negative for issuance
}

// Available is what this account can spend. Locked funds have moved to an
// escrow account, so there is nothing to subtract.
func (a AccountBalance) Available() int64 { return a.Balance }

// IssuanceAccount is the counter-account value is issued against.
func IssuanceAccount(ccy string) string { return issuanceAccountPrefix + ccy }

// EscrowAccount holds a transfer's locked value while it is prepared.
func EscrowAccount(transferID string) string { return escrowAccountPrefix + transferID }

// journal posts entries and keeps the materialised balances in step, within
// one invocation.
//
// It carries the same write-through cache the balances needed before, for the
// same reason: Fabric's GetState does not see this transaction's own PutState,
// so two entries touching one account would otherwise each read the
// pre-transaction value and the second would discard the first.
type journal struct {
	ctx   contractapi.TransactionContextInterface
	cache map[string]AccountBalance
	n     int // entry index within this transaction
}

func newJournal(ctx contractapi.TransactionContextInterface) *journal {
	return &journal{ctx: ctx, cache: map[string]AccountBalance{}}
}

func accountKey(ctx contractapi.TransactionContextInterface, account, ccy string) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(accountKeyPrefix, []string{account, ccy})
	if err != nil {
		return "", fmt.Errorf("build account key: %w", err)
	}
	return key, nil
}

// balance returns this transaction's view: what it has already posted,
// otherwise what is committed. An account with no history reads as zero.
func (j *journal) balance(account, ccy string) (AccountBalance, error) {
	if err := requireNonEmpty("account", account); err != nil {
		return AccountBalance{}, err
	}
	if err := requireCcy("ccy", ccy); err != nil {
		return AccountBalance{}, err
	}
	key, err := accountKey(j.ctx, account, ccy)
	if err != nil {
		return AccountBalance{}, err
	}
	if cached, ok := j.cache[key]; ok {
		return cached, nil
	}
	raw, err := j.ctx.GetStub().GetState(key)
	if err != nil {
		return AccountBalance{}, fmt.Errorf("read account %s/%s: %w", account, ccy, err)
	}
	if raw == nil {
		return AccountBalance{Account: account, Ccy: ccy}, nil
	}
	var b AccountBalance
	if err := json.Unmarshal(raw, &b); err != nil {
		return AccountBalance{}, fmt.Errorf("decode account %s/%s: %w", account, ccy, err)
	}
	b.Balance = int64(b.Credits) - int64(b.Debits)
	return b, nil
}

func (j *journal) putBalance(b AccountBalance) error {
	b.Balance = int64(b.Credits) - int64(b.Debits)
	key, err := accountKey(j.ctx, b.Account, b.Ccy)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode account %s/%s: %w", b.Account, b.Ccy, err)
	}
	if err := j.ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write account %s/%s: %w", b.Account, b.Ccy, err)
	}
	j.cache[key] = b
	return nil
}

// post appends one balanced entry and folds it into both accounts.
//
// It refuses to overdraw a real account. Issuance accounts are exempt: going
// negative is what they are for, and the size of that negative number is the
// amount of value outstanding.
func (j *journal) post(kind, ref, debit, credit, ccy string, amount uint64) error {
	if amount == 0 {
		return fmt.Errorf("a journal entry must move a non-zero amount")
	}
	if debit == credit {
		return fmt.Errorf("entry would debit and credit the same account %q", debit)
	}

	from, err := j.balance(debit, ccy)
	if err != nil {
		return err
	}
	if !isIssuanceAccount(debit) && from.Balance < int64(amount) {
		return fmt.Errorf("account %s has %d available %s, needs %d",
			debit, from.Balance, ccy, amount)
	}
	to, err := j.balance(credit, ccy)
	if err != nil {
		return err
	}

	newDebits, err := addU64(from.Debits, amount)
	if err != nil {
		return fmt.Errorf("debit %s: %w", debit, err)
	}
	newCredits, err := addU64(to.Credits, amount)
	if err != nil {
		return fmt.Errorf("credit %s: %w", credit, err)
	}
	// Balance is a signed difference, so each side has to stay inside the
	// signed range or the subtraction would wrap and report a nonsense
	// position. MaxInt64 minor units is about 92 quadrillion paise; the bound
	// exists so the type cannot betray us, not because anyone will reach it.
	if newDebits > math.MaxInt64 {
		return fmt.Errorf("debiting %s would exceed the representable range", debit)
	}
	if newCredits > math.MaxInt64 {
		return fmt.Errorf("crediting %s would exceed the representable range", credit)
	}
	from.Debits = newDebits
	to.Credits = newCredits

	if err := j.putBalance(from); err != nil {
		return err
	}
	if err := j.putBalance(to); err != nil {
		return err
	}

	at, err := txTime(j.ctx)
	if err != nil {
		return err
	}
	entry := Entry{
		EntryID: j.ctx.GetStub().GetTxID() + ":" + strconv.Itoa(j.n),
		TxID:    j.ctx.GetStub().GetTxID(),
		Ref:     ref,
		Kind:    kind,
		Debit:   debit,
		Credit:  credit,
		Ccy:     ccy,
		Amount:  amount,
		At:      at,
	}
	j.n++

	key, err := j.ctx.GetStub().CreateCompositeKey(entryKeyPrefix, []string{entry.EntryID})
	if err != nil {
		return fmt.Errorf("build entry key: %w", err)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode entry: %w", err)
	}
	if err := j.ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write entry: %w", err)
	}
	return nil
}

func isIssuanceAccount(account string) bool {
	return len(account) > len(issuanceAccountPrefix) &&
		account[:len(issuanceAccountPrefix)] == issuanceAccountPrefix
}

func isReservedAccount(account string) bool {
	if isIssuanceAccount(account) {
		return true
	}
	return len(account) > len(escrowAccountPrefix) &&
		account[:len(escrowAccountPrefix)] == escrowAccountPrefix
}
