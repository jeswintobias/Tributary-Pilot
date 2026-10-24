// Command settlement is the Tributary Pilot settlement chaincode.
//
// It implements the Phase 0 single-ledger two-phase commit described in
// docs/SETTLEMENT_FLOW.md. Deploy it with network/deploy-cc.sh, which sets the
// endorsement policy that requires both SenderOrg and PayoutBankOrg to endorse a
// Commit.
package main

import (
	"log"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"

	"github.com/tributary-pilot/chaincode/settlement/settlement"
)

func main() {
	cc, err := contractapi.NewChaincode(&settlement.SmartContract{})
	if err != nil {
		log.Panicf("create settlement chaincode: %v", err)
	}
	if err := cc.Start(); err != nil {
		log.Panicf("start settlement chaincode: %v", err)
	}
}
