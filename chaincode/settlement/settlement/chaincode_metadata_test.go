package settlement

import (
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
