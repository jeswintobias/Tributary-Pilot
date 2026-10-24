// Package settlement implements the Phase 0 single-ledger two-phase commit for
// Tributary Pilot. See docs/SETTLEMENT_FLOW.md for the state machine this
// contract enforces.
//
// Determinism rules observed throughout this package:
//   - time comes from stub.GetTxTimestamp(), never from the wall clock
//   - no randomness
//   - no map iteration in anything that reaches the write set or an event
//   - no external calls
package settlement

// State is a transfer's position in the two-phase commit state machine.
//
// The only permitted transitions are:
//
//	CAPTURED -> ACCEPTED -> PREPARED -> SETTLED
//	CAPTURED -> ABORTED
//	ACCEPTED -> ABORTED
//	PREPARED -> ABORTED
//
// Two transitions are deliberately absent. CAPTURED -> SETTLED would commit a
// leg that was never locked. CAPTURED -> PREPARED would let the sending
// organisation lock the payout organisation's funds without its agreement, so
// an explicit Accept by the payout organisation stands between them.
const (
	StateCaptured = "CAPTURED"
	StateAccepted = "ACCEPTED"
	StatePrepared = "PREPARED"
	StateSettled  = "SETTLED"
	StateAborted  = "ABORTED"
)

// MaxSettlementWindowSeconds bounds how far ahead a transfer may expire.
//
// Without an upper bound, a sending organisation could create a transfer
// expiring years from now and — once accepted — lock the counterparty's funds
// for that whole period, because a prepared transfer can only be aborted after
// it expires. Fifteen minutes matches the lifetime of a signed FX quote, which
// is the real constraint on how long a transfer's terms stay meaningful.
const MaxSettlementWindowSeconds int64 = 900

// Event names emitted on each state change. Phase 1's event listener drives the
// payout adapter and the treasury dashboard from these.
const (
	EventCaptured = "TransferCaptured"
	EventAccepted = "TransferAccepted"
	EventPrepared = "TransferPrepared"
	EventSettled  = "TransferSettled"
	EventAborted  = "TransferAborted"
)

// Transfer is one remittance with two legs that must settle together or not at
// all.
//
// Amounts are integer minor units — fils for AED, paise for INR, cents for USD.
// There is no floating-point amount anywhere in this contract.
//
// The two legs are:
//
//	sender leg: SenderOrg pays SenderLegAmount SenderCcy to PayoutOrg
//	payout leg: PayoutOrg pays PayoutLegAmount PayoutCcy to PayoutPayeeRef
//
// PayoutPayeeRef is an opaque reference to the beneficiary — a salted hash
// computed off-chain. No personally identifying information is ever written to
// the ledger.
type Transfer struct {
	TransferID string `json:"transferID"`

	SenderOrg       string `json:"senderOrg"` // MSP ID that pays the sender leg
	SenderLegAmount uint64 `json:"senderLegAmount"`
	SenderCcy       string `json:"senderCcy"`

	PayoutOrg       string `json:"payoutOrg"` // MSP ID that pays the payout leg
	PayoutLegAmount uint64 `json:"payoutLegAmount"`
	PayoutCcy       string `json:"payoutCcy"`
	PayoutPayeeRef  string `json:"payoutPayeeRef"` // salted hash of the payee, never PII

	QuoteID   string `json:"quoteID"`
	ExpiresAt int64  `json:"expiresAt"` // Unix seconds; bounded by the FX quote's own expiry

	State string `json:"state"`

	// ProofHash is anchored at Prepare over the transfer's canonical fields and
	// re-checked at Commit. In Phase 1 this is where a counterparty proof from
	// the foreign ledger is verified instead.
	ProofHash string `json:"proofHash"`

	CreatedAt  int64 `json:"createdAt"`
	AcceptedAt int64 `json:"acceptedAt"`
	PreparedAt int64 `json:"preparedAt"`
	ResolvedAt int64 `json:"resolvedAt"` // set when SETTLED or ABORTED
}

// Balance is one account's holding in one currency, in integer minor units.
//
// The account is an MSP ID for an institution, or an opaque payee reference for
// a beneficiary. Available funds are Total - Locked, and the invariant
// 0 <= Locked <= Total holds after every operation.
//
// Phase 0 keeps this minimal map inside the settlement contract. Phase 1
// replaces it with the token and pool contracts; the state machine does not
// change when it does.
type Balance struct {
	Account string `json:"account"`
	Ccy     string `json:"ccy"`
	Total   uint64 `json:"total"`
	Locked  uint64 `json:"locked"`
}

// Available returns the unlocked portion of the balance.
func (b Balance) Available() uint64 {
	if b.Locked > b.Total {
		// Unreachable while the invariant holds; guard against underflow rather
		// than wrapping to a huge number if it ever breaks.
		return 0
	}
	return b.Total - b.Locked
}

// stateChangeEvent is the payload emitted on every state change. Fields are a
// fixed struct, not a map, so the serialised bytes are identical on every
// endorsing peer.
type stateChangeEvent struct {
	TransferID      string `json:"transferID"`
	State           string `json:"state"`
	SenderOrg       string `json:"senderOrg"`
	SenderLegAmount uint64 `json:"senderLegAmount"`
	SenderCcy       string `json:"senderCcy"`
	PayoutOrg       string `json:"payoutOrg"`
	PayoutLegAmount uint64 `json:"payoutLegAmount"`
	PayoutCcy       string `json:"payoutCcy"`
	PayoutPayeeRef  string `json:"payoutPayeeRef"`
	ProofHash       string `json:"proofHash"`
	At              int64  `json:"at"`
}
