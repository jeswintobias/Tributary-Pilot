package settlement

import (
	"strings"
	"testing"

	"github.com/hyperledger/fabric-chaincode-go/v2/pkg/cid"
	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
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
	events    map[string][]byte // committed events
	pendingEv map[string][]byte
	now       int64

	// getStateErr, when set, makes GetState for that exact key fail.
	getStateErr map[string]error
}

func newMockStub(now int64) *mockStub {
	return &mockStub{
		committed:   map[string][]byte{},
		pending:     map[string][]byte{},
		events:      map[string][]byte{},
		pendingEv:   map[string][]byte{},
		now:         now,
		getStateErr: map[string]error{},
	}
}

// GetState reads committed state only — never this transaction's own writes.
func (m *mockStub) GetState(key string) ([]byte, error) {
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

func (m *mockStub) GetTxID() string { return "mock-tx" }

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

// ledger is one shared state map that several differently-signed contexts can
// act on, which is how we simulate organisations transacting on one channel.
type ledger struct {
	t    *testing.T
	stub *mockStub
	sc   *SmartContract
}

func newLedger(t *testing.T, now int64) *ledger {
	t.Helper()
	return &ledger{t: t, stub: newMockStub(now), sc: &SmartContract{}}
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

func (l *ledger) create(org, transferID string, senderAmt uint64, senderCcy string,
	payoutAmt uint64, payoutCcy, quote string, expires int64, payoutOrg, payee string) error {
	return l.invoke(org, func(c contractapi.TransactionContextInterface) error {
		return l.sc.CreateTransfer(c, transferID, senderAmt, senderCcy,
			payoutAmt, payoutCcy, quote, expires, payoutOrg, payee)
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

func (l *ledger) balance(account, ccy string) Balance {
	l.t.Helper()
	b, err := l.sc.GetBalance(l.as(account), account, ccy)
	if err != nil {
		l.t.Fatalf("GetBalance(%s, %s): %v", account, ccy, err)
	}
	return *b
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
