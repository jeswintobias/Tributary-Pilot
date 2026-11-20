package settlement

import (
	"reflect"
	"testing"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// TestContract_IsAcceptedByContractAPI catches signature problems that only
// surface when the chaincode starts — an unsupported parameter type, for
// instance — rather than at deploy time on the live network.
func TestContract_IsAcceptedByContractAPI(t *testing.T) {
	cc, err := contractapi.NewChaincode(&SmartContract{})
	if err != nil {
		t.Fatalf("contract rejected by the contract API: %v", err)
	}
	if cc == nil {
		t.Fatal("NewChaincode returned a nil chaincode and no error")
	}
}

// The gap this closes.
//
// Every other test in this package calls the contract's Go methods directly,
// which skips the layer the PEER goes through: the contract API derives a JSON
// schema from each parameter type and validates arguments against it before
// the method is reached. A parameter struct carrying fields a caller cannot
// supply is accepted by every unit test here and refused by the live network.
//
// That is not hypothetical. ProposePolicy originally took the stored Policy
// type, so the schema demanded `approvedBy` and `version` from the proposer —
// fields that only exist once a change has been approved. It failed on the
// network with "value did not match schema" after passing every test in this
// package.
//
// The generated metadata is not reachable from here (ContractChaincode keeps
// it unexported), so this checks the invariant underneath it instead: a type
// used as an INPUT must not carry fields that only the contract fills in.
// That is the property that was actually violated, and it is the one worth
// pinning.
//
// Worth recording honestly: []VoucherSplit is also an input and does carry
// fields filled in later (redeemed, redeemedAt, redeemedBy), and it was
// accepted on the live network. Why a slice of structs behaves differently
// from a struct has not been established, so nothing here asserts a rule about
// it — the Policy case is the one that is known.
func TestInputTypes_CarryNoFieldsOnlyTheContractFillsIn(t *testing.T) {
	// Fields that only exist once something has happened. A caller proposing a
	// change cannot know any of them.
	filledInLater := map[string]bool{
		"Version": true, "ActivatedAt": true, "ProposedBy": true, "ApprovedBy": true,
	}

	terms := reflect.TypeOf(PolicyTerms{})
	for i := 0; i < terms.NumField(); i++ {
		name := terms.Field(i).Name
		if filledInLater[name] {
			t.Errorf("PolicyTerms carries %q, which is only set at activation; "+
				"the contract API will demand it from the proposer", name)
		}
	}

	// And the stored type must still carry them, or the activation record
	// would be lost.
	policy := reflect.TypeOf(Policy{})
	for field := range filledInLater {
		if _, ok := policy.FieldByName(field); !ok {
			t.Errorf("Policy no longer records %q", field)
		}
	}

	// The terms must reach the stored type, not be duplicated beside it.
	if _, ok := policy.FieldByName("PolicyTerms"); !ok {
		t.Error("Policy no longer embeds PolicyTerms, so the two can drift apart")
	}
}

// ProposePolicy must take the input type, not the stored one. This is the
// signature that was wrong.
func TestProposePolicy_TakesTermsNotTheStoredPolicy(t *testing.T) {
	method, ok := reflect.TypeOf(&SmartContract{}).MethodByName("ProposePolicy")
	if !ok {
		t.Fatal("ProposePolicy is missing")
	}
	// receiver, ctx, terms
	if method.Type.NumIn() != 3 {
		t.Fatalf("ProposePolicy takes %d arguments, expected a context and terms", method.Type.NumIn()-1)
	}
	if got := method.Type.In(2); got != reflect.TypeOf(PolicyTerms{}) {
		t.Errorf("ProposePolicy takes %v; it must take PolicyTerms, or the contract "+
			"API will demand activation fields from the proposer", got)
	}
}
