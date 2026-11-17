package settlement

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-chaincode-go/v2/pkg/cid"
	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The mocks below embed the interface they stand in for and override only the
// methods this contract actually calls. Anything else panics with a nil-pointer
// dereference, which is the behaviour we want: if the contract starts calling a
// stub method we have not thought about, a test fails loudly instead of quietly
// returning a zero value.

// mockStub reproduces Fabric's transaction semantics, which are easy to get
// wrong in a mock and which this contract depends on:
//
//   - PutState does NOT update the state a later GetState in the SAME
//     transaction reads. Fabric's txSimulator.SetState only appends to the
//     transaction's write set, while queryExecutor.GetState reads the state
//     database directly. There is no read-your-writes.
//   - A transaction that returns an error commits nothing at all; its whole
//     write set is discarded.
//
// An earlier version of this mock applied writes immediately, which made a real
// double-credit bug in Commit invisible to every test. Keep these semantics.
type mockStub struct {
	shim.ChaincodeStubInterface

	committed map[string][]byte // what GetState sees
	pending   map[string][]byte // this transaction's write set
	reads     map[string]bool   // this transaction's read set
	events    map[string][]byte // committed events
	pendingEv map[string][]byte
	now       int64
	txSeq     int // distinct transaction IDs, as a real chain gives

	// getStateErr, when set, makes GetState for that exact key fail.
	getStateErr map[string]error
}

func newMockStub(now int64) *mockStub {
	return &mockStub{
		committed:   map[string][]byte{},
		pending:     map[string][]byte{},
		reads:       map[string]bool{},
		events:      map[string][]byte{},
		pendingEv:   map[string][]byte{},
		now:         now,
		getStateErr: map[string]error{},
	}
}

// GetState reads committed state only — never this transaction's own writes.
//
// Every key read is recorded. Fabric's MVCC check compares a transaction's read
// set against the versions committed by the time it is validated, so the read
// set is what decides whether two transactions can run concurrently. Tracking
// it lets a test assert non-conflict directly instead of inferring it.
func (m *mockStub) GetState(key string) ([]byte, error) {
	m.reads[key] = true
	if err, ok := m.getStateErr[key]; ok {
		return nil, err
	}
	v, ok := m.committed[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

// PutState appends to the write set. It becomes visible only once the
// transaction commits.
func (m *mockStub) PutState(key string, value []byte) error {
	buf := make([]byte, len(value))
	copy(buf, value)
	m.pending[key] = buf
	return nil
}

func (m *mockStub) DelState(key string) error {
	m.pending[key] = nil
	return nil
}

// CreateCompositeKey mirrors Fabric's own encoding closely enough for key
// uniqueness, which is all this contract depends on.
func (m *mockStub) CreateCompositeKey(objectType string, attributes []string) (string, error) {
	return "\x00" + objectType + "\x00" + strings.Join(attributes, "\x00") + "\x00", nil
}

func (m *mockStub) GetTxTimestamp() (*timestamppb.Timestamp, error) {
	return &timestamppb.Timestamp{Seconds: m.now}, nil
}

func (m *mockStub) SetEvent(name string, payload []byte) error {
	m.pendingEv[name] = payload
	return nil
}

// GetTxID gives each transaction its own ID. Journal entry keys are built from
// it, so a constant would make two transactions write the same entry key and
// the second would silently overwrite the first — hiding exactly the kind of
// bug the journal exists to make impossible.
func (m *mockStub) GetTxID() string { return fmt.Sprintf("mock-tx-%d", m.txSeq) }

// drunixUnpaginatedRangeLimit reproduces a real defect in the platform.
//
// On DRUNIX's SQL state database, the UNPAGINATED range scan delegates to the
// paginated one with a hardcoded page size of 10, where upstream Fabric passes
// 0 for "no limit". So GetStateByPartialCompositeKey silently returns at most
// ten rows: no error, no truncation flag, just a short answer.
//
// The mock reproduces it on purpose. A mock that is kinder than the platform
// is how a real defect passes CI — this contract has been bitten by exactly
// that before (see T6.4). Any code that goes back to the unpaginated call will
// now fail a test here rather than on a live ledger with sixteen accounts.
const drunixUnpaginatedRangeLimit = 10

// GetStateByPartialCompositeKey ranges over COMMITTED state in key order, as
// Fabric does — this transaction's own writes are not visible to it either —
// and truncates at ten, as DRUNIX does.
func (m *mockStub) GetStateByPartialCompositeKey(objectType string, keys []string) (shim.StateQueryIteratorInterface, error) {
	matched := m.matchingKeys(objectType, keys)
	if len(matched) > drunixUnpaginatedRangeLimit {
		matched = matched[:drunixUnpaginatedRangeLimit]
	}
	return &mockIterator{stub: m, keys: matched}, nil
}

// GetStateByPartialCompositeKeyWithPagination honours the page size it is
// given, which is what makes the workaround in iterate.go work.
func (m *mockStub) GetStateByPartialCompositeKeyWithPagination(
	objectType string, keys []string, pageSize int32, bookmark string,
) (shim.StateQueryIteratorInterface, *peer.QueryResponseMetadata, error) {
	matched := m.matchingKeys(objectType, keys)

	// The bookmark is the key to resume AFTER, which is how Fabric's own
	// bookmarks behave for a range query.
	start := 0
	if bookmark != "" {
		start = sort.SearchStrings(matched, bookmark)
	}
	if start > len(matched) {
		start = len(matched)
	}
	end := len(matched)
	if pageSize > 0 && start+int(pageSize) < end {
		end = start + int(pageSize)
	}
	page := matched[start:end]

	next := ""
	if end < len(matched) {
		next = matched[end]
	}
	return &mockIterator{stub: m, keys: page},
		&peer.QueryResponseMetadata{
			FetchedRecordsCount: int32(len(page)),
			Bookmark:            next,
		}, nil
}

func (m *mockStub) matchingKeys(objectType string, keys []string) []string {
	prefix := "\x00" + objectType + "\x00" + strings.Join(keys, "\x00")
	matched := make([]string, 0, len(m.committed))
	for k := range m.committed {
		if strings.HasPrefix(k, prefix) {
			matched = append(matched, k)
			m.reads[k] = true
		}
	}
	// Fabric returns a range in key order; sorting keeps the mock faithful and
	// the iteration deterministic.
	sort.Strings(matched)
	return matched
}

type mockIterator struct {
	shim.StateQueryIteratorInterface
	stub *mockStub
	keys []string
	i    int
}

func (it *mockIterator) HasNext() bool { return it.i < len(it.keys) }

func (it *mockIterator) Next() (*queryresult.KV, error) {
	if !it.HasNext() {
		return nil, fmt.Errorf("iterator exhausted")
	}
	k := it.keys[it.i]
	it.i++
	return &queryresult.KV{Key: k, Value: it.stub.committed[k]}, nil
}

func (it *mockIterator) Close() error { return nil }

// commitTx applies the write set, as a peer does for a valid transaction.
func (m *mockStub) commitTx() {
	for k, v := range m.pending {
		if v == nil {
			delete(m.committed, k)
			continue
		}
		m.committed[k] = v
	}
	for k, v := range m.pendingEv {
		m.events[k] = v
	}
	m.resetTx()
}

// discardTx throws the write set away, as a peer does for a transaction whose
// chaincode returned an error.
func (m *mockStub) discardTx() { m.resetTx() }

func (m *mockStub) resetTx() {
	m.pending = map[string][]byte{}
	m.pendingEv = map[string][]byte{}
	m.reads = map[string]bool{}
	m.txSeq++
}

// touched is every key this transaction read or wrote: its MVCC footprint. Two
// transactions can be endorsed concurrently without either being invalidated
// only if one's writes do not meet the other's footprint.
func (m *mockStub) touched() map[string]bool {
	out := map[string]bool{}
	for k := range m.reads {
		out[k] = true
	}
	for k := range m.pending {
		out[k] = true
	}
	return out
}

func (m *mockStub) writtenKeys() map[string]bool {
	out := map[string]bool{}
	for k := range m.pending {
		out[k] = true
	}
	return out
}

type mockClientIdentity struct {
	cid.ClientIdentity
	mspID string
}

func (m *mockClientIdentity) GetMSPID() (string, error) { return m.mspID, nil }

type mockContext struct {
	contractapi.TransactionContextInterface
	stub     *mockStub
	identity *mockClientIdentity
}

func (m *mockContext) GetStub() shim.ChaincodeStubInterface  { return m.stub }
func (m *mockContext) GetClientIdentity() cid.ClientIdentity { return m.identity }

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// contractCtx is the transaction context, named short because the test files
// pass it to a closure on nearly every line.
type contractCtx = contractapi.TransactionContextInterface

// ledger is one shared state map that several differently-signed contexts can
// act on, which is how we simulate organisations transacting on one channel.
type ledger struct {
	t    *testing.T
	stub *mockStub
	sc   *SmartContract

	// oracleKey signs the quotes create() mints. Generated per ledger so no
	// test depends on a fixed key, and registered on the chain at setup
	// because CreateTransfer now refuses an unsigned rate.
	oracleKey *ecdsa.PrivateKey
	oracleID  string

	// autoAttest issues the beneficiary's KYC and sanctions attestations
	// inside create(). Settlement is gated on them, so without this every test
	// about expiry, access control or arithmetic would have to set up
	// compliance it is not testing. Tests that ARE about the gate set this
	// false and attest, or fail to attest, deliberately.
	autoAttest bool
}

func newLedger(t *testing.T, now int64) *ledger {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate oracle key: %v", err)
	}
	l := &ledger{
		t: t, stub: newMockStub(now), sc: &SmartContract{},
		oracleKey: key, oracleID: "oracle-test", autoAttest: true,
	}
	if err := l.registerOracle(l.oracleID, key); err != nil {
		t.Fatalf("register oracle: %v", err)
	}
	return l
}

// quoteValiditySeconds is how long the harness's minted quotes last. Long
// enough that no test trips over it accidentally, short enough to stay
// realistic for an FX rate.
const quoteValiditySeconds int64 = 300

// attestorOrg is the only MSP allowed to issue attestations, and the only one
// allowed to register an oracle key. Both are deliberately the same
// organisation here: PlatformOrg is the platform node.
const attestorOrg = "Org3MSP"

func publicKeyPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func (l *ledger) registerOracle(oracleID string, key *ecdsa.PrivateKey) error {
	l.t.Helper()
	return l.invoke(attestorOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.RegisterOracle(c, oracleID, publicKeyPEM(l.t, key))
	})
}

// sign produces the signature an oracle would send with a quote.
func (l *ledger) sign(q Quote, key *ecdsa.PrivateKey) string {
	l.t.Helper()
	digest := sha256.Sum256(QuoteBytes(q))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		l.t.Fatalf("sign quote: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// quoteFor builds a quote whose rate is exactly the transfer's two legs, so
// the slippage check passes with no tolerance at all. A test that wants a
// wrong rate builds its own.
func (l *ledger) quoteFor(quoteID, senderCcy string, senderAmt uint64,
	payoutCcy string, payoutAmt uint64, expires int64) Quote {
	return Quote{
		QuoteID: quoteID, OracleID: l.oracleID,
		BaseCcy: senderCcy, QuoteCcy: payoutCcy,
		RateNum: payoutAmt, RateDen: senderAmt,
		ExpiresAt: expires, MaxSlippageBps: 0,
	}
}

// attest issues both required attestations for a beneficiary.
func (l *ledger) attest(subject string) error {
	l.t.Helper()
	for _, kind := range RequiredAttestations {
		err := l.invoke(attestorOrg, func(c contractapi.TransactionContextInterface) error {
			return l.sc.IssueAttestation(c, subject, kind, l.stub.now+86_400, "case-file")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *ledger) revoke(subject, kind, reason string) error {
	l.t.Helper()
	return l.invoke(attestorOrg, func(c contractapi.TransactionContextInterface) error {
		return l.sc.RevokeAttestation(c, subject, kind, reason)
	})
}

// as returns a transaction context signed by the given MSP ID, sharing the one
// underlying state map.
func (l *ledger) as(mspID string) contractapi.TransactionContextInterface {
	return &mockContext{stub: l.stub, identity: &mockClientIdentity{mspID: mspID}}
}

// at moves the simulated transaction timestamp. Every endorsing peer would see
// this same value, which is the property GetTxTimestamp() gives us on a real
// network.
func (l *ledger) at(unixSeconds int64) { l.stub.now = unixSeconds }

// invoke runs one state-changing call as one transaction: the write set is
// applied on success and thrown away on error. Every test goes through this, so
// no test can accidentally rely on writes that Fabric would have discarded.
func (l *ledger) invoke(org string, call func(contractapi.TransactionContextInterface) error) error {
	err := call(l.as(org))
	if err != nil {
		l.stub.discardTx()
	} else {
		l.stub.commitTx()
	}
	return err
}

func (l *ledger) seed(org, ccy string, amount uint64) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SeedBalance(c, ccy, amount)
	})
}

// create mints a signed quote at the exact rate the two legs imply, attests
// the beneficiary unless the test has turned that off, and creates the
// transfer.
//
// Two details worth knowing before reading a test that uses it:
//
// The quote is dealt PER TRANSFER — the caller's quoteID is combined with the
// transfer ID — because a quote is single use on the chain. That matches how a
// dealt rate works in practice, and it means a test passing one quoteID for
// several transfers still gets a valid quote for each instead of tripping a
// rule it is not about. TestCreateTransfer_RefusesAReusedQuote covers the rule
// itself, through createWith.
//
// The quote's own expiry is independent of the transfer's. They are different
// clocks: a rate stops being true long before a transfer stops being settleable,
// and tying them together made a test about transfer expiry fail on quote
// expiry instead.
func (l *ledger) create(org, transferID string, senderAmt uint64, senderCcy string,
	payoutAmt uint64, payoutCcy, quoteID string, expires int64, payoutOrg, payee string) error {
	l.t.Helper()
	if l.autoAttest && !isReservedAccount(payee) && payee != "" {
		// Best effort: a malformed payee is the point of some tests, and the
		// attestation failing there must not mask the error being tested.
		_ = l.attest(payee)
	}
	q := l.quoteFor(quoteID+":"+transferID, senderCcy, senderAmt,
		payoutCcy, payoutAmt, l.stub.now+quoteValiditySeconds)
	sig := l.sign(q, l.oracleKey)
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.CreateTransfer(c, transferID, senderAmt, senderCcy,
			payoutAmt, payoutCcy, q, sig, expires, payoutOrg, payee)
	})
}

// createWith creates a transfer with a caller-built quote and signature, for
// tests about the quote itself.
func (l *ledger) createWith(org, transferID string, senderAmt uint64, senderCcy string,
	payoutAmt uint64, payoutCcy string, q Quote, sig string, expires int64, payoutOrg, payee string) error {
	l.t.Helper()
	if l.autoAttest && payee != "" {
		_ = l.attest(payee)
	}
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.CreateTransfer(c, transferID, senderAmt, senderCcy,
			payoutAmt, payoutCcy, q, sig, expires, payoutOrg, payee)
	})
}

func (l *ledger) accept(org, transferID string) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.Accept(c, transferID)
	})
}

func (l *ledger) prepare(org, transferID string) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.Prepare(c, transferID)
	})
}

func (l *ledger) commit(org, transferID string) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.Commit(c, transferID)
	})
}

func (l *ledger) abort(org, transferID string) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.Abort(c, transferID)
	})
}

func (l *ledger) balance(account, ccy string) AccountBalance {
	l.t.Helper()
	b, err := l.sc.GetBalance(l.as(account), account, ccy)
	if err != nil {
		l.t.Fatalf("GetBalance(%s, %s): %v", account, ccy, err)
	}
	return *b
}

// locked is what a transfer currently holds in escrow.
func (l *ledger) locked(transferID, ccy string) int64 {
	l.t.Helper()
	return l.balance(EscrowAccount(transferID), ccy).Balance
}

// assertBooksBalance folds every account on the ledger. Because each entry
// applies one amount to both a debit and a credit, every currency must sum to
// exactly zero — including issuance, which carries the negative of everything
// outstanding. A non-zero sum means value was created or destroyed.
func (l *ledger) assertBooksBalance() {
	l.t.Helper()
	accounts, err := l.sc.ListBalances(l.as(senderOrg))
	if err != nil {
		l.t.Fatalf("ListBalances: %v", err)
	}
	perCcy := map[string]int64{}
	for _, a := range accounts {
		perCcy[a.Ccy] += a.Balance
	}
	for ccy, sum := range perCcy {
		if sum != 0 {
			l.t.Fatalf("books do not balance in %s: accounts sum to %d, want 0", ccy, sum)
		}
	}
}

func (l *ledger) transfer(id string) Transfer {
	l.t.Helper()
	tr, err := l.sc.GetTransfer(l.as(senderOrg), id)
	if err != nil {
		l.t.Fatalf("GetTransfer(%s): %v", id, err)
	}
	return *tr
}

func (l *ledger) lastEvent(name string) []byte { return l.stub.events[name] }

// ---------------------------------------------------------------------------
// Netting harness
// ---------------------------------------------------------------------------

// thirdOrg is a third institution, so the multilateral cases have somewhere for
// value to pass through rather than merely back and forth.
const thirdOrg = "PlatformOrgMSP"

func (l *ledger) submit(org, obligationID, creditor, ccy string, amount uint64) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.SubmitObligation(c, obligationID, creditor, ccy, amount)
	})
}

func (l *ledger) closeWindow(org, ccy string, seq int64) (*Window, error) {
	var w *Window
	err := l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		var innerErr error
		w, innerErr = l.sc.CloseWindow(c, ccy, seq)
		return innerErr
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (l *ledger) exposure(account, ccy string) uint64 {
	l.t.Helper()
	e, err := l.sc.GetExposure(l.as(account), account, ccy)
	if err != nil {
		l.t.Fatalf("GetExposure(%s, %s): %v", account, ccy, err)
	}
	return e.Outstanding
}

func (l *ledger) window(ccy string, seq int64) Window {
	l.t.Helper()
	w, err := l.sc.GetWindow(l.as(senderOrg), ccy, seq)
	if err != nil {
		l.t.Fatalf("GetWindow(%s, %d): %v", ccy, seq, err)
	}
	return *w
}

// seqNow is the window a submission at the ledger's current simulated time
// lands in.
func (l *ledger) seqNow() int64 { return windowSeq(l.stub.now) }

// footprint runs one call and reports its MVCC footprint: every key it read or
// wrote, and separately every key it wrote. The call must succeed.
//
// This is how the concurrency claims in pool_test.go are checked. Rather than
// asserting that a design "avoids contention", a test can take two
// transactions' footprints and show that neither writes anything the other
// touched — which is precisely the condition under which Fabric endorses both
// without invalidating either.
func (l *ledger) footprint(org string, call func(contractapi.TransactionContextInterface) error) (touched, written map[string]bool) {
	l.t.Helper()
	if err := call(l.as(org)); err != nil {
		l.stub.discardTx()
		l.t.Fatalf("call as %s failed: %v", org, err)
	}
	touched, written = l.stub.touched(), l.stub.writtenKeys()
	l.stub.commitTx()
	return touched, written
}

// keyKind reports the object type a composite key belongs to, so a test can say
// which kind of state a transaction touched.
func keyKind(key string) string {
	parts := strings.Split(key, "\x00")
	if len(parts) < 2 {
		return key
	}
	return parts[1]
}

func overlap(a, b map[string]bool) []string {
	var shared []string
	for k := range a {
		if b[k] {
			shared = append(shared, keyKind(k))
		}
	}
	sort.Strings(shared)
	return shared
}

// probe runs one call, records its MVCC footprint and then throws the write set
// away, leaving the ledger exactly as it was.
//
// This is what lets two transactions be compared as if they had been endorsed
// against the same state, which is the situation MVCC actually arbitrates. A
// comparison that committed the first call before running the second would be
// measuring a sequence, not a race.
func (l *ledger) probe(org string, call func(contractapi.TransactionContextInterface) error) (touched, written map[string]bool) {
	l.t.Helper()
	if err := call(l.as(org)); err != nil {
		l.stub.discardTx()
		l.t.Fatalf("probe as %s failed: %v", org, err)
	}
	touched, written = l.stub.touched(), l.stub.writtenKeys()
	l.stub.discardTx()
	return touched, written
}
