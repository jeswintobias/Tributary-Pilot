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

type mockStub struct {
	shim.ChaincodeStubInterface

	state  map[string][]byte
	events map[string][]byte
	now    int64

	// getStateErr, when set, makes the next GetState for that exact key fail.
	getStateErr map[string]error
}

func newMockStub(now int64) *mockStub {
	return &mockStub{
		state:       map[string][]byte{},
		events:      map[string][]byte{},
		now:         now,
		getStateErr: map[string]error{},
	}
}

func (m *mockStub) GetState(key string) ([]byte, error) {
	if err, ok := m.getStateErr[key]; ok {
		return nil, err
	}
	v, ok := m.state[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (m *mockStub) PutState(key string, value []byte) error {
	// Copy so a later mutation of the caller's slice cannot reach stored state.
	buf := make([]byte, len(value))
	copy(buf, value)
	m.state[key] = buf
	return nil
}

func (m *mockStub) DelState(key string) error {
	delete(m.state, key)
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
	m.events[name] = payload
	return nil
}

func (m *mockStub) GetTxID() string { return "mock-tx" }

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
// act on, which is how we simulate two organisations transacting on one channel.
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
