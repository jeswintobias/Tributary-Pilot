package settlement

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// Netting-first settlement.
//
// THE PROBLEM THIS SOLVES
//
// The two-phase commit in contract.go is correct and atomic, and it does not
// scale. Every transfer reads and writes the payer's balance key, so two
// transfers from one institution in the same block both read the same version
// of that key and the second is thrown out at validation with
// MVCC_READ_CONFLICT. A shared liquidity pool makes it worse: every transfer in
// the corridor touches the pool's key, so the whole corridor serialises down to
// roughly one transfer per block no matter how many peers are added. This is
// the hot-key problem, and it is the reason "blockchain settlement" demos tend
// to quote throughput from a single-threaded run.
//
// Adding peers cannot fix it. The conflict is on the key, not the CPU.
//
// HOW THIS PATH AVOIDS IT
//
// A transfer stops touching any shared key. Submitting an obligation writes
// exactly two keys:
//
//	oblig:<ccy>:<seq>:<obligationID>   unique to that obligation, so it can
//	                                   never conflict with anything
//	expo:<ccy>:<account>               the submitter's own exposure record,
//	                                   which only the submitter ever writes
//
// Neither is shared between institutions, so Org1 and Org2 submit in parallel
// at full speed. Writes to the shared balance keys drop from two per transfer
// to at most one per participating account per window — a bound that does not
// grow with volume.
//
// WHY THE WINDOW NUMBER IS NOT A KEY
//
// The obvious design keeps a "current window" pointer in state. That pointer
// would be read by every submission and written when the window rolls, which
// recreates the hot key it was meant to remove.
//
// Instead the window is derived arithmetically from the transaction's own
// timestamp: seq = txTime / NettingWindowSeconds. Every endorsing peer sees the
// same proposal timestamp and so computes the same window, with no read, no
// write and no coordination. The window number is a function of time, not a
// piece of state.
//
// WHY CLOSING A WINDOW IS SAFE
//
// CloseWindow ranges over a window's obligations, and a range query inside a
// writing transaction is normally dangerous: Fabric records the range in the
// read set, and another transaction inserting a key into it invalidates the
// close. That is a phantom read, and it is why ListTransfers is marked
// query-only.
//
// Here it cannot happen. A window may only be closed once seq is strictly in
// the past, and a submission always lands in the window its own timestamp
// names. No transaction can ever write into a past window's key range, so the
// range is immutable before the close reads it. Time does the locking, so there
// is nothing to lock.
//
// WHAT IS NETTED, AND WHAT IS NOT
//
// This path is for the many small transfers that make up a remittance corridor,
// where value flows both ways and most of it cancels. The two-phase commit
// stays for high-value payment-versus-payment, where the parties need
// settlement finality on the individual transfer rather than on a batch. Both
// write through the same double-entry journal, so the books balance across the
// two paths together.

const (
	obligationKeyPrefix = "oblig"
	exposureKeyPrefix   = "expo"
	windowKeyPrefix     = "window"
)

// NettingWindowSeconds is how long a netting window accrues before it may be
// closed.
//
// Sixty seconds is a demo figure chosen so a live run shows several windows in
// a few minutes. A real corridor would use the operator's cycle — NPCI's
// deferred net settlement runs on fixed daily cycles, CLS on a daily one — and
// the only thing that changes is this constant, because nothing else in the
// design knows how long a window is.
const NettingWindowSeconds int64 = 60

// Window states.
const (
	WindowOpen    = "OPEN"    // accruing, or past but not yet closed
	WindowSettled = "SETTLED" // netted and posted to the journal
)

// Obligation is one institution's undertaking to pay another, recorded without
// moving any value.
//
// It is not a transfer of funds and not a lock. It is a claim that is netted
// against every other claim in its window, and only the residue moves.
type Obligation struct {
	ObligationID string `json:"obligationID"`
	Seq          int64  `json:"seq"` // the window this belongs to
	Debtor       string `json:"debtor"`
	Creditor     string `json:"creditor"`
	Ccy          string `json:"ccy"`
	Amount       uint64 `json:"amount"` // integer minor units
	At           int64  `json:"at"`
}

// Exposure is how much value an account has committed to windows that have not
// settled yet.
//
// It spans windows on purpose. A window that is past but not yet closed still
// holds unsettled commitments, and an account that could ignore them would be
// able to commit the same funds twice by waiting for the window to roll. The
// journal subtracts this from a balance before allowing any debit, so no path
// — netting or two-phase commit — can spend value that is already promised.
type Exposure struct {
	Account     string `json:"account"`
	Ccy         string `json:"ccy"`
	Outstanding uint64 `json:"outstanding"`
}

// Window is the settled record of one netting cycle, written once when the
// window closes. It is also the idempotency guard: a window that already has a
// record cannot be closed again.
//
// GrossValue and NetValue are the headline measurement. Gross is what would
// have crossed the pool if every obligation had settled on its own; net is what
// actually moved. The difference is liquidity the corridor did not have to
// find.
type Window struct {
	Ccy   string `json:"ccy"`
	Seq   int64  `json:"seq"`
	State string `json:"state"`

	Obligations int    `json:"obligations"`
	GrossValue  uint64 `json:"grossValue"`
	NetValue    uint64 `json:"netValue"`

	// Postings is how many journal movements the close produced, against
	// 2*Obligations for the unnetted path. This is the write-amplification
	// figure on the shared balance keys.
	Postings int `json:"postings"`

	// CompressionBps is (1 - Net/Gross) in basis points, so the ratio stays an
	// integer. 10000 means everything cancelled; 0 means nothing did.
	CompressionBps int64 `json:"compressionBps"`

	OpenedAt int64  `json:"openedAt"`
	ClosedAt int64  `json:"closedAt"`
	ClosedBy string `json:"closedBy"`
}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

// SubmitObligation records that the caller owes a counterparty an amount, to be
// netted at the end of the current window.
//
// The caller's MSP ID is the debtor, read from the signature: an institution
// cannot commit someone else's funds. Nothing moves here and no shared key is
// touched, which is the whole point.
//
// The cover check is against the caller's own exposure and balance only, so it
// reads nothing another institution writes.
func (s *SmartContract) SubmitObligation(
	ctx contractapi.TransactionContextInterface,
	obligationID string,
	creditor string,
	ccy string,
	amount uint64,
) error {
	if err := requireID("obligationID", obligationID, maxIDLen); err != nil {
		return err
	}
	if err := requireID("creditor", creditor, maxIDLen); err != nil {
		return err
	}
	if err := requireCcy("ccy", ccy); err != nil {
		return err
	}
	if amount == 0 {
		return fmt.Errorf("amount must be greater than zero")
	}
	// The journal's counter-accounts are not parties. A creditor posing as
	// issuance: would be credited from a liability account that is allowed to go
	// negative without limit.
	if isReservedAccount(creditor) {
		return fmt.Errorf("creditor %q uses a reserved account prefix", creditor)
	}

	debtor, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if debtor == creditor {
		return fmt.Errorf("debtor and creditor are both %q", debtor)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	seq := windowSeq(now)

	key, err := obligationKey(ctx, ccy, seq, obligationID)
	if err != nil {
		return err
	}
	existing, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read obligation %q: %w", obligationID, err)
	}
	if existing != nil {
		return fmt.Errorf("obligation %q already exists in window %d: replays are rejected", obligationID, seq)
	}

	// Cover. Exposure is checked against the gross amount committed, not the net
	// position, because the net is not known until the window closes and the
	// incoming side of it is written by other institutions' keys — which this
	// transaction must not read if it is to stay conflict-free.
	//
	// The consequence is deliberate and worth stating plainly: netting as
	// implemented here compresses the value that MOVES at settlement, not the
	// collateral an institution has to hold while the window is open. Freeing
	// that collateral needs bilateral credit limits, so that cover is
	// gross-out minus an agreed line rather than gross-out in full. That is a
	// policy question with a counterparty-risk answer, and it is on the
	// roadmap; it is not something to quietly assume.
	j := newJournal(ctx)
	expo, err := j.exposure(debtor, ccy)
	if err != nil {
		return err
	}
	committed, err := addU64(expo.Outstanding, amount)
	if err != nil {
		return fmt.Errorf("exposure for %s: %w", debtor, err)
	}
	if committed > math.MaxInt64 {
		return fmt.Errorf("exposure for %s would exceed the representable range", debtor)
	}
	bal, err := j.balance(debtor, ccy)
	if err != nil {
		return err
	}
	if bal.Balance < int64(committed) {
		return fmt.Errorf(
			"account %s holds %d %s and has %d already committed, cannot commit %d more",
			debtor, bal.Balance, ccy, expo.Outstanding, amount)
	}

	expo.Outstanding = committed
	if err := j.setExposure(expo); err != nil {
		return err
	}

	ob := Obligation{
		ObligationID: obligationID,
		Seq:          seq,
		Debtor:       debtor,
		Creditor:     creditor,
		Ccy:          ccy,
		Amount:       amount,
		At:           now,
	}
	raw, err := json.Marshal(ob)
	if err != nil {
		return fmt.Errorf("encode obligation %q: %w", obligationID, err)
	}
	if err := ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write obligation %q: %w", obligationID, err)
	}
	return emitObligation(ctx, EventObligationSubmitted, ob)
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

// CloseWindow nets a past window and posts only the residue to the journal.
//
// Any member of the channel may call it. The netting is a pure function of
// state that is already immutable, so every caller and every endorsing peer
// computes byte-identical results — there is no advantage to being the one who
// closes, and so no privileged role to grant, revoke or lose.
func (s *SmartContract) CloseWindow(
	ctx contractapi.TransactionContextInterface,
	ccy string,
	seq int64,
) (*Window, error) {
	if err := requireCcy("ccy", ccy); err != nil {
		return nil, err
	}
	if seq < 0 {
		return nil, fmt.Errorf("seq %d must not be negative", seq)
	}
	caller, err := callerMSPID(ctx)
	if err != nil {
		return nil, err
	}
	now, err := txTime(ctx)
	if err != nil {
		return nil, err
	}

	// The window must be in the past. This is what makes the range query below
	// safe: no transaction can write into a window its own timestamp does not
	// name, so a past window's key range is already frozen.
	current := windowSeq(now)
	if seq >= current {
		return nil, fmt.Errorf(
			"window %d is still open (current window is %d at tx time %d): "+
				"a window may only be closed once it is in the past", seq, current, now)
	}

	wKey, err := windowKey(ctx, ccy, seq)
	if err != nil {
		return nil, err
	}
	if existing, err := ctx.GetStub().GetState(wKey); err != nil {
		return nil, fmt.Errorf("read window %s/%d: %w", ccy, seq, err)
	} else if existing != nil {
		return nil, fmt.Errorf("window %s/%d is already settled: replays are rejected", ccy, seq)
	}

	obs, err := windowObligations(ctx, ccy, seq)
	if err != nil {
		return nil, err
	}

	w := Window{
		Ccy:         ccy,
		Seq:         seq,
		State:       WindowSettled,
		Obligations: len(obs),
		OpenedAt:    seq * NettingWindowSeconds,
		ClosedAt:    now,
		ClosedBy:    caller,
	}

	gross := uint64(0)
	for _, ob := range obs {
		gross, err = addU64(gross, ob.Amount)
		if err != nil {
			return nil, fmt.Errorf("window %s/%d gross: %w", ccy, seq, err)
		}
	}
	w.GrossValue = gross

	postings, err := pairPostings(netPositions(obs))
	if err != nil {
		return nil, fmt.Errorf("window %s/%d netting: %w", ccy, seq, err)
	}

	// Release each debtor's committed gross BEFORE posting the net movements,
	// and not after. The journal refuses a debit that would eat into an
	// account's reservation, and this window's own gross is part of that
	// reservation until it is released — so posting first would have the close
	// blocked by the very obligations it is settling. An institution that
	// committed its whole balance gross would deadlock on a net of almost
	// nothing.
	//
	// Releasing first is safe in the other direction too. Submission already
	// established balance >= exposure, and net <= gross, so after the release
	// the available balance is at least gross, which is at least the net about
	// to be debited. The close cannot overdraw.
	j := newJournal(ctx)
	if err := j.releaseExposures(ccy, obs); err != nil {
		return nil, err
	}

	net := uint64(0)
	for _, p := range postings {
		if err := j.post(KindNet, windowRef(ccy, seq), p.Debit, p.Credit, ccy, p.Amount); err != nil {
			return nil, fmt.Errorf("post net movement %s->%s: %w", p.Debit, p.Credit, err)
		}
		net, err = addU64(net, p.Amount)
		if err != nil {
			return nil, fmt.Errorf("window %s/%d net: %w", ccy, seq, err)
		}
	}
	w.NetValue = net
	w.Postings = len(postings)
	if gross > 0 {
		// Integer basis points, computed so the division happens last.
		w.CompressionBps = int64(10000 - (net*10000)/gross)
	}

	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("encode window %s/%d: %w", ccy, seq, err)
	}
	if err := ctx.GetStub().PutState(wKey, raw); err != nil {
		return nil, fmt.Errorf("write window %s/%d: %w", ccy, seq, err)
	}
	if err := emitWindow(ctx, w); err != nil {
		return nil, err
	}
	return &w, nil
}

// ---------------------------------------------------------------------------
// Netting
// ---------------------------------------------------------------------------

// netPosition is one account's signed position across a whole window: what it
// is owed minus what it owes.
type netPosition struct {
	Account string `json:"account"`
	Net     int64  `json:"net"` // positive for a creditor, negative for a debtor
}

// posting is one movement the close will make.
type posting struct {
	Debit  string
	Credit string
	Amount uint64
}

// netPositions folds a window's obligations into one signed position per
// account.
//
// The result is sorted by account name. Chaincode runs on every endorsing peer
// and Go randomises map iteration order, so folding into a map and ranging over
// it would produce a different write-set ordering on each peer and the
// endorsements would not match. The sort is what makes this deterministic.
//
// Because every obligation contributes exactly +amount to one account and
// -amount to another, the positions always sum to zero.
func netPositions(obs []Obligation) []netPosition {
	net := map[string]int64{}
	for _, ob := range obs {
		net[ob.Debtor] -= int64(ob.Amount)
		net[ob.Creditor] += int64(ob.Amount)
	}
	accounts := make([]string, 0, len(net))
	for a := range net {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)

	out := make([]netPosition, 0, len(accounts))
	for _, a := range accounts {
		if net[a] == 0 {
			// An account that owes exactly what it is owed needs no movement at
			// all. This is netting paying off in its purest form.
			continue
		}
		out = append(out, netPosition{Account: a, Net: net[a]})
	}
	return out
}

// pairPostings turns net positions into the movements that realise them.
//
// Debtors are matched against creditors largest-first. Each step closes out at
// least one of the two accounts it touches, so the number of postings is at
// most debtors+creditors-1 — against two postings per obligation on the
// unnetted path.
//
// Ties are broken by account name so the output is byte-identical on every
// endorsing peer.
func pairPostings(pos []netPosition) ([]posting, error) {
	type side struct {
		account string
		amount  uint64 // magnitude
	}
	var debtors, creditors []side
	for _, p := range pos {
		if p.Net < 0 {
			debtors = append(debtors, side{p.Account, uint64(-p.Net)})
		} else if p.Net > 0 {
			creditors = append(creditors, side{p.Account, uint64(p.Net)})
		}
	}
	byAmountThenName := func(s []side) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].amount != s[j].amount {
				return s[i].amount > s[j].amount
			}
			return s[i].account < s[j].account
		}
	}
	sort.SliceStable(debtors, byAmountThenName(debtors))
	sort.SliceStable(creditors, byAmountThenName(creditors))

	out := []posting{}
	i, k := 0, 0
	for i < len(debtors) && k < len(creditors) {
		amount := debtors[i].amount
		if creditors[k].amount < amount {
			amount = creditors[k].amount
		}
		out = append(out, posting{
			Debit:  debtors[i].account,
			Credit: creditors[k].account,
			Amount: amount,
		})
		debtors[i].amount -= amount
		creditors[k].amount -= amount
		if debtors[i].amount == 0 {
			i++
		}
		if creditors[k].amount == 0 {
			k++
		}
	}
	// Positions sum to zero by construction, so both sides must run out
	// together. If they have not, the fold above is wrong and posting a partial
	// result would create or destroy value — refuse instead.
	if i != len(debtors) || k != len(creditors) {
		return nil, fmt.Errorf(
			"net positions do not sum to zero: %d debtors and %d creditors left unmatched",
			len(debtors)-i, len(creditors)-k)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetWindow returns a settled window's record, or the live shape of a window
// that has not been closed yet.
//
// Query-only: it ranges over obligations, so nothing that writes may call it
// except CloseWindow, which is safe for the reason given at the top of this
// file.
func (s *SmartContract) GetWindow(
	ctx contractapi.TransactionContextInterface,
	ccy string,
	seq int64,
) (*Window, error) {
	if err := requireCcy("ccy", ccy); err != nil {
		return nil, err
	}
	wKey, err := windowKey(ctx, ccy, seq)
	if err != nil {
		return nil, err
	}
	raw, err := ctx.GetStub().GetState(wKey)
	if err != nil {
		return nil, fmt.Errorf("read window %s/%d: %w", ccy, seq, err)
	}
	if raw != nil {
		var w Window
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("decode window %s/%d: %w", ccy, seq, err)
		}
		return &w, nil
	}

	// Not settled: report what it would net to if it were closed now. This is
	// the projection the treasury view wants, and computing it as a query means
	// an operator can see the compression before committing to it.
	obs, err := windowObligations(ctx, ccy, seq)
	if err != nil {
		return nil, err
	}
	w := Window{
		Ccy:         ccy,
		Seq:         seq,
		State:       WindowOpen,
		Obligations: len(obs),
		OpenedAt:    seq * NettingWindowSeconds,
	}
	for _, ob := range obs {
		w.GrossValue, err = addU64(w.GrossValue, ob.Amount)
		if err != nil {
			return nil, fmt.Errorf("window %s/%d gross: %w", ccy, seq, err)
		}
	}
	postings, err := pairPostings(netPositions(obs))
	if err != nil {
		return nil, err
	}
	for _, p := range postings {
		w.NetValue, err = addU64(w.NetValue, p.Amount)
		if err != nil {
			return nil, fmt.Errorf("window %s/%d net: %w", ccy, seq, err)
		}
	}
	w.Postings = len(postings)
	if w.GrossValue > 0 {
		w.CompressionBps = int64(10000 - (w.NetValue*10000)/w.GrossValue)
	}
	return &w, nil
}

// CurrentWindow returns the window sequence a transaction submitted now would
// land in. Clients need it to know which window to close next.
func (s *SmartContract) CurrentWindow(ctx contractapi.TransactionContextInterface) (int64, error) {
	now, err := txTime(ctx)
	if err != nil {
		return 0, err
	}
	return windowSeq(now), nil
}

// ListWindows returns every settled window, in key order. Query-only.
func (s *SmartContract) ListWindows(ctx contractapi.TransactionContextInterface) ([]*Window, error) {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey(windowKeyPrefix, []string{})
	if err != nil {
		return nil, fmt.Errorf("range over windows: %w", err)
	}
	defer iter.Close()

	out := []*Window{}
	for iter.HasNext() {
		kv, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("read next window: %w", err)
		}
		var w Window
		if err := json.Unmarshal(kv.GetValue(), &w); err != nil {
			return nil, fmt.Errorf("decode window at %q: %w", kv.GetKey(), err)
		}
		out = append(out, &w)
	}
	return out, nil
}

// GetExposure returns how much an account has committed to windows that have
// not settled.
func (s *SmartContract) GetExposure(
	ctx contractapi.TransactionContextInterface,
	account, ccy string,
) (*Exposure, error) {
	if err := requireNonEmpty("account", account); err != nil {
		return nil, err
	}
	if err := requireCcy("ccy", ccy); err != nil {
		return nil, err
	}
	e, err := readExposure(ctx, account, ccy)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListObligations returns a window's obligations, in key order. Query-only.
func (s *SmartContract) ListObligations(
	ctx contractapi.TransactionContextInterface,
	ccy string,
	seq int64,
) ([]*Obligation, error) {
	if err := requireCcy("ccy", ccy); err != nil {
		return nil, err
	}
	obs, err := windowObligations(ctx, ccy, seq)
	if err != nil {
		return nil, err
	}
	out := make([]*Obligation, 0, len(obs))
	for i := range obs {
		out = append(out, &obs[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// windowSeq maps a transaction timestamp to a window number. Integer division,
// so every peer that sees the same proposal timestamp lands in the same window
// without reading any state.
func windowSeq(now int64) int64 { return now / NettingWindowSeconds }

func windowRef(ccy string, seq int64) string {
	return fmt.Sprintf("window:%s:%d", ccy, seq)
}

// seqAttr renders a sequence number fixed-width so that composite keys sort in
// numeric order. Without the padding "window 10" would range before "window 9"
// and ListWindows would report cycles out of order.
func seqAttr(seq int64) string { return fmt.Sprintf("%019d", seq) }

func obligationKey(ctx contractapi.TransactionContextInterface, ccy string, seq int64, id string) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(obligationKeyPrefix, []string{ccy, seqAttr(seq), id})
	if err != nil {
		return "", fmt.Errorf("build obligation key: %w", err)
	}
	return key, nil
}

func windowKey(ctx contractapi.TransactionContextInterface, ccy string, seq int64) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(windowKeyPrefix, []string{ccy, seqAttr(seq)})
	if err != nil {
		return "", fmt.Errorf("build window key: %w", err)
	}
	return key, nil
}

func exposureKey(ctx contractapi.TransactionContextInterface, account, ccy string) (string, error) {
	key, err := ctx.GetStub().CreateCompositeKey(exposureKeyPrefix, []string{account, ccy})
	if err != nil {
		return "", fmt.Errorf("build exposure key: %w", err)
	}
	return key, nil
}

// windowObligations reads one window's obligations in key order.
//
// Safe inside CloseWindow only because that function has already established
// that the window is in the past and therefore immutable. See the phantom-read
// note at the top of this file.
func windowObligations(ctx contractapi.TransactionContextInterface, ccy string, seq int64) ([]Obligation, error) {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey(
		obligationKeyPrefix, []string{ccy, seqAttr(seq)})
	if err != nil {
		return nil, fmt.Errorf("range over obligations in %s/%d: %w", ccy, seq, err)
	}
	defer iter.Close()

	out := []Obligation{}
	for iter.HasNext() {
		kv, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("read next obligation: %w", err)
		}
		var ob Obligation
		if err := json.Unmarshal(kv.GetValue(), &ob); err != nil {
			return nil, fmt.Errorf("decode obligation at %q: %w", kv.GetKey(), err)
		}
		out = append(out, ob)
	}
	return out, nil
}

func readExposure(ctx contractapi.TransactionContextInterface, account, ccy string) (Exposure, error) {
	key, err := exposureKey(ctx, account, ccy)
	if err != nil {
		return Exposure{}, err
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return Exposure{}, fmt.Errorf("read exposure %s/%s: %w", account, ccy, err)
	}
	if raw == nil {
		return Exposure{Account: account, Ccy: ccy}, nil
	}
	var e Exposure
	if err := json.Unmarshal(raw, &e); err != nil {
		return Exposure{}, fmt.Errorf("decode exposure %s/%s: %w", account, ccy, err)
	}
	return e, nil
}

func putExposure(ctx contractapi.TransactionContextInterface, e Exposure) error {
	key, err := exposureKey(ctx, e.Account, e.Ccy)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode exposure %s/%s: %w", e.Account, e.Ccy, err)
	}
	if err := ctx.GetStub().PutState(key, raw); err != nil {
		return fmt.Errorf("write exposure %s/%s: %w", e.Account, e.Ccy, err)
	}
	return nil
}

// releaseExposures gives each debtor back the gross it committed to this
// window.
//
// Totals are folded per debtor first and written once, in sorted order, so that
// a debtor appearing in several obligations produces one write rather than
// several — and so the write set is identical on every endorsing peer.
func (j *journal) releaseExposures(ccy string, obs []Obligation) error {
	gross := map[string]uint64{}
	for _, ob := range obs {
		sum, err := addU64(gross[ob.Debtor], ob.Amount)
		if err != nil {
			return fmt.Errorf("exposure release for %s: %w", ob.Debtor, err)
		}
		gross[ob.Debtor] = sum
	}
	debtors := make([]string, 0, len(gross))
	for d := range gross {
		debtors = append(debtors, d)
	}
	sort.Strings(debtors)

	for _, d := range debtors {
		e, err := j.exposure(d, ccy)
		if err != nil {
			return err
		}
		if e.Outstanding < gross[d] {
			// Exposure was recorded when each obligation was submitted, so it
			// cannot be smaller than the sum of them. If it is, the records
			// disagree and continuing would silently release liquidity that was
			// never reserved.
			return fmt.Errorf(
				"exposure for %s/%s is %d but window committed %d: records disagree",
				d, ccy, e.Outstanding, gross[d])
		}
		e.Outstanding -= gross[d]
		if err := j.setExposure(e); err != nil {
			return err
		}
	}
	return nil
}

func emitObligation(ctx contractapi.TransactionContextInterface, name string, ob Obligation) error {
	payload, err := json.Marshal(ob)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if err := ctx.GetStub().SetEvent(name, payload); err != nil {
		return fmt.Errorf("emit %s event: %w", name, err)
	}
	return nil
}

func emitWindow(ctx contractapi.TransactionContextInterface, w Window) error {
	payload, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", EventWindowSettled, err)
	}
	if err := ctx.GetStub().SetEvent(EventWindowSettled, payload); err != nil {
		return fmt.Errorf("emit %s event: %w", EventWindowSettled, err)
	}
	return nil
}
