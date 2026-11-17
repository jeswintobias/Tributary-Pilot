package settlement

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"math/bits"
	"strconv"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
)

// Signed FX quotes.
//
// THE HOLE THIS FILLS
//
// Until now CreateTransfer took a quoteID that was a string and nothing more.
// It was stored, folded into the proof hash, and never checked against
// anything. So the sending organisation chose both legs of the transfer and
// named a rate nobody verified: 2,200 AED for 1 INR was as acceptable to the
// contract as the real rate, and so was a rate that had been true yesterday.
//
// The counterparty's protection was that it had to Accept. That is a real
// control and it is not enough — it makes the payout bank re-derive the rate
// off-chain for every transfer and trust its own copy of the market, which is
// exactly the reconciliation this project exists to remove.
//
// WHAT A QUOTE IS NOW
//
// An oracle signs a quote off-chain. The chaincode verifies the signature, the
// expiry and the arithmetic, so a transfer can only exist at a rate somebody
// put their name to, recently, within a stated tolerance.
//
// RATES ARE RATIONAL, NOT DECIMAL
//
// A rate is an integer numerator over an integer denominator. There is no
// decimal rate anywhere, for the same reason there is no floating-point
// amount: two endorsing peers must compute bit-identical results, and
// 1 AED = 22.7431 INR cannot be written as a float that every peer rounds the
// same way. 227431/10000 can.
//
// WHY VERIFICATION IS SAFE IN CHAINCODE
//
// Signing is not deterministic — ECDSA draws a nonce — but signing happens off
// chain. Verification is a pure function of (public key, message, signature)
// and returns the same answer on every peer, which is all the determinism rule
// requires.

const (
	oracleKeyPrefix   = "oracle"
	quoteUsedPrefix   = "quoteused"
	maxSignatureBytes = 256
	maxPubKeyBytes    = 2048
)

// OracleMSPs may register a signing key. Registration is what makes a key
// trusted, so the set of organisations that can do it is the root of this
// whole mechanism.
//
// A constant rather than a registry, deliberately, and it is the weakest part
// of this design: production wants the set in the channel configuration, where
// changing it needs the ordinary config-update signatures rather than a
// chaincode upgrade. Hard-coding it means an oracle cannot be added or removed
// without redeploying. That is stated here rather than discovered later.
var OracleMSPs = map[string]bool{
	"Org3MSP": true, // PlatformOrg, the NPCI platform node
}

// Quote is a signed statement that one currency was worth another at a moment,
// for a bounded time, within a bounded tolerance.
type Quote struct {
	QuoteID  string `json:"quoteID"`
	OracleID string `json:"oracleID"`

	BaseCcy  string `json:"baseCcy"`  // the currency the sender pays
	QuoteCcy string `json:"quoteCcy"` // the currency the beneficiary receives

	// RateNum/RateDen expresses QuoteCcy per BaseCcy as an exact rational.
	// 1 AED = 22.7431 INR is 227431/10000.
	RateNum uint64 `json:"rateNum"`
	RateDen uint64 `json:"rateDen"`

	// ExpiresAt bounds how long the quote may be used, checked against the
	// transaction timestamp rather than any wall clock.
	ExpiresAt int64 `json:"expiresAt"`

	// MaxSlippageBps is how far the transfer's actual legs may sit from the
	// quoted rate, in basis points. Zero means exact.
	MaxSlippageBps uint64 `json:"maxSlippageBps"`
}

// OracleKey is a registered signing key.
type OracleKey struct {
	OracleID     string `json:"oracleID"`
	PublicKeyPEM string `json:"publicKeyPEM"`
	RegisteredBy string `json:"registeredBy"`
	RegisteredAt int64  `json:"registeredAt"`
	Revoked      bool   `json:"revoked"`
	RevokedAt    int64  `json:"revokedAt"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// RegisterOracle records a public key that may sign quotes.
//
// Only an organisation in OracleMSPs may call it, and a key may not be
// silently replaced: re-registering an existing oracle ID is refused, because
// a quote verified yesterday under one key would otherwise fail to verify
// today under another, and nobody would be able to tell whether the quote or
// the key had changed.
func (s *SmartContract) RegisterOracle(
	ctx contractapi.TransactionContextInterface,
	oracleID string,
	publicKeyPEM string,
) error {
	if err := requireID("oracleID", oracleID, maxIDLen); err != nil {
		return err
	}
	if len(publicKeyPEM) == 0 || len(publicKeyPEM) > maxPubKeyBytes {
		return fmt.Errorf("publicKeyPEM must be between 1 and %d bytes", maxPubKeyBytes)
	}
	mspID, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if !OracleMSPs[mspID] {
		return fmt.Errorf("%s may not register an oracle key", mspID)
	}
	// Parse before storing. A key that cannot be parsed would make every quote
	// it signed fail verification with a confusing error, long after the
	// mistake was made.
	if _, err := parsePublicKey(publicKeyPEM); err != nil {
		return fmt.Errorf("publicKeyPEM is not a usable ECDSA public key: %w", err)
	}

	key, err := ctx.GetStub().CreateCompositeKey(oracleKeyPrefix, []string{oracleID})
	if err != nil {
		return fmt.Errorf("build oracle key: %w", err)
	}
	existing, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read oracle %q: %w", oracleID, err)
	}
	if existing != nil {
		return fmt.Errorf("oracle %q is already registered: rotate by registering a new ID", oracleID)
	}

	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(OracleKey{
		OracleID: oracleID, PublicKeyPEM: publicKeyPEM,
		RegisteredBy: mspID, RegisteredAt: now,
	})
	if err != nil {
		return fmt.Errorf("encode oracle %q: %w", oracleID, err)
	}
	return ctx.GetStub().PutState(key, raw)
}

// RevokeOracle stops a key being accepted for new transfers.
//
// Transfers already created keep their recorded quote: revoking a key says
// "do not trust this for anything new", not "unwind what was settled on it".
// Retroactively invalidating settled transfers would be a far worse property
// than a compromised key, and is not something a settlement system may do.
func (s *SmartContract) RevokeOracle(ctx contractapi.TransactionContextInterface, oracleID string) error {
	mspID, err := callerMSPID(ctx)
	if err != nil {
		return err
	}
	if !OracleMSPs[mspID] {
		return fmt.Errorf("%s may not revoke an oracle key", mspID)
	}
	key, oracle, err := loadOracle(ctx, oracleID)
	if err != nil {
		return err
	}
	if oracle.Revoked {
		return fmt.Errorf("oracle %q is already revoked", oracleID)
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	oracle.Revoked = true
	oracle.RevokedAt = now
	raw, err := json.Marshal(oracle)
	if err != nil {
		return fmt.Errorf("encode oracle %q: %w", oracleID, err)
	}
	return ctx.GetStub().PutState(key, raw)
}

// GetOracle returns a registered key.
func (s *SmartContract) GetOracle(ctx contractapi.TransactionContextInterface, oracleID string) (*OracleKey, error) {
	_, oracle, err := loadOracle(ctx, oracleID)
	if err != nil {
		return nil, err
	}
	return &oracle, nil
}

// ListOracles returns every registered key, in key order. Query-only.
func (s *SmartContract) ListOracles(ctx contractapi.TransactionContextInterface) ([]*OracleKey, error) {
	out := []*OracleKey{}
	err := forEachInRange(ctx, oracleKeyPrefix, []string{}, func(kv *queryresult.KV) error {
		var o OracleKey
		if err := json.Unmarshal(kv.GetValue(), &o); err != nil {
			return fmt.Errorf("decode oracle at %q: %w", kv.GetKey(), err)
		}
		out = append(out, &o)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func loadOracle(ctx contractapi.TransactionContextInterface, oracleID string) (string, OracleKey, error) {
	if err := requireNonEmpty("oracleID", oracleID); err != nil {
		return "", OracleKey{}, err
	}
	key, err := ctx.GetStub().CreateCompositeKey(oracleKeyPrefix, []string{oracleID})
	if err != nil {
		return "", OracleKey{}, fmt.Errorf("build oracle key: %w", err)
	}
	raw, err := ctx.GetStub().GetState(key)
	if err != nil {
		return "", OracleKey{}, fmt.Errorf("read oracle %q: %w", oracleID, err)
	}
	if raw == nil {
		return "", OracleKey{}, fmt.Errorf("oracle %q is not registered", oracleID)
	}
	var o OracleKey
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", OracleKey{}, fmt.Errorf("decode oracle %q: %w", oracleID, err)
	}
	return key, o, nil
}

// ---------------------------------------------------------------------------
// Canonical bytes
// ---------------------------------------------------------------------------

// QuoteBytes is the exact byte sequence an oracle signs.
//
// Length-prefixed so concatenation is unambiguous. Without it a quote for
// "AED"/"INR" and one for "AE"/"DINR" would produce the same message, and a
// signature over one would verify the other — the classic field-splitting
// forgery.
//
// Exported because the oracle service has to produce the identical bytes. Two
// implementations of one encoding is a place where a mismatch hides, so the
// test suite signs with the Go implementation and the service's own test
// vectors are checked against it.
func QuoteBytes(q Quote) []byte {
	var buf []byte
	for _, field := range []string{
		"tributary-fx-quote-v1", // domain separator: these bytes are a quote
		q.QuoteID,
		q.OracleID,
		q.BaseCcy,
		q.QuoteCcy,
		strconv.FormatUint(q.RateNum, 10),
		strconv.FormatUint(q.RateDen, 10),
		strconv.FormatInt(q.ExpiresAt, 10),
		strconv.FormatUint(q.MaxSlippageBps, 10),
	} {
		buf = append(buf, []byte(strconv.Itoa(len(field)))...)
		buf = append(buf, ':')
		buf = append(buf, []byte(field)...)
	}
	return buf
}

func parsePublicKey(pemText string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, not ECDSA", parsed)
	}
	return pub, nil
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// verifyQuote checks a quote's signature, its expiry, and that the transfer's
// two legs actually sit at the quoted rate.
//
// All three matter and none substitutes for another. A valid signature on a
// stale quote is yesterday's rate. A fresh signed quote attached to legs that
// do not match it is a rate nobody quoted, wearing a signature that happens to
// verify.
func verifyQuote(
	ctx contractapi.TransactionContextInterface,
	q Quote,
	signatureB64 string,
	senderCcy string, senderAmount uint64,
	payoutCcy string, payoutAmount uint64,
	now int64,
) error {
	if err := requireID("quoteID", q.QuoteID, maxIDLen); err != nil {
		return err
	}
	if err := requireID("oracleID", q.OracleID, maxIDLen); err != nil {
		return err
	}
	if err := requireCcy("baseCcy", q.BaseCcy); err != nil {
		return err
	}
	if err := requireCcy("quoteCcy", q.QuoteCcy); err != nil {
		return err
	}
	if q.RateNum == 0 || q.RateDen == 0 {
		return fmt.Errorf("rate %d/%d is not a usable rate", q.RateNum, q.RateDen)
	}
	if q.MaxSlippageBps > 10000 {
		return fmt.Errorf("maxSlippageBps %d exceeds 100%%", q.MaxSlippageBps)
	}

	// The quote must be about this transfer's currencies, or it is a signature
	// over something else entirely.
	if q.BaseCcy != senderCcy || q.QuoteCcy != payoutCcy {
		return fmt.Errorf(
			"quote is %s->%s but the transfer is %s->%s",
			q.BaseCcy, q.QuoteCcy, senderCcy, payoutCcy)
	}

	if now >= q.ExpiresAt {
		return fmt.Errorf(
			"quote %q expired at %d (tx time %d): an FX rate is only true for a moment",
			q.QuoteID, q.ExpiresAt, now)
	}

	_, oracle, err := loadOracle(ctx, q.OracleID)
	if err != nil {
		return err
	}
	if oracle.Revoked {
		return fmt.Errorf("oracle %q was revoked at %d", q.OracleID, oracle.RevokedAt)
	}

	if len(signatureB64) == 0 || len(signatureB64) > maxSignatureBytes*2 {
		return fmt.Errorf("signature must be between 1 and %d characters", maxSignatureBytes*2)
	}
	signature, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("signature is not valid base64: %w", err)
	}
	pub, err := parsePublicKey(oracle.PublicKeyPEM)
	if err != nil {
		return fmt.Errorf("oracle %q has an unusable key: %w", q.OracleID, err)
	}
	digest := sha256.Sum256(QuoteBytes(q))
	if !ecdsa.VerifyASN1(pub, digest[:], signature) {
		return fmt.Errorf("quote %q is not signed by oracle %q", q.QuoteID, q.OracleID)
	}

	return checkLegsAgainstRate(q, senderAmount, payoutAmount)
}

// checkLegsAgainstRate is where a signed quote stops being decoration.
//
// The expected payout is senderAmount * RateNum / RateDen, and the actual
// payout leg has to sit within MaxSlippageBps of it. Without this a caller
// could attach a perfectly valid quote to legs bearing no relation to it.
func checkLegsAgainstRate(q Quote, senderAmount, payoutAmount uint64) error {
	expected, err := mulDiv(senderAmount, q.RateNum, q.RateDen)
	if err != nil {
		return fmt.Errorf("applying rate %d/%d to %d: %w", q.RateNum, q.RateDen, senderAmount, err)
	}
	if expected == 0 {
		return fmt.Errorf(
			"rate %d/%d applied to %d rounds to zero: the sender leg is too small for this rate",
			q.RateNum, q.RateDen, senderAmount)
	}

	tolerance, err := mulDiv(expected, q.MaxSlippageBps, 10000)
	if err != nil {
		return fmt.Errorf("computing tolerance: %w", err)
	}

	var drift uint64
	if payoutAmount > expected {
		drift = payoutAmount - expected
	} else {
		drift = expected - payoutAmount
	}
	if drift > tolerance {
		return fmt.Errorf(
			"payout leg %d is %d away from the quoted %d, which exceeds the %d allowed at %d bps",
			payoutAmount, drift, expected, tolerance, q.MaxSlippageBps)
	}
	return nil
}

// mulDiv computes a*b/c exactly, keeping the intermediate product in 128 bits.
//
// a*b overflows uint64 for amounts and rates this contract permits — 2,200 AED
// in fils times a rate numerator passes 2^64 easily — and an overflowed
// intermediate would produce a rate check that silently passes anything.
func mulDiv(a, b, c uint64) (uint64, error) {
	if c == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= c {
		// The quotient would not fit in 64 bits, and bits.Div64 panics rather
		// than returning an error, so this has to be caught here.
		return 0, fmt.Errorf("%d * %d / %d does not fit in 64 bits", a, b, c)
	}
	q, _ := bits.Div64(hi, lo, c)
	if q > math.MaxInt64 {
		return 0, fmt.Errorf("%d * %d / %d exceeds the representable range", a, b, c)
	}
	return q, nil
}

// consumeQuote records that a quote has been used, and refuses a second use.
//
// The record is keyed by quote ID alone, so it is also the idempotency guard
// for the quote: two transfers cannot be created on one dealt rate even if
// they are created by different organisations in different blocks.
func consumeQuote(ctx contractapi.TransactionContextInterface, quoteID, transferID string, now int64) error {
	key, err := ctx.GetStub().CreateCompositeKey(quoteUsedPrefix, []string{quoteID})
	if err != nil {
		return fmt.Errorf("build quote-used key: %w", err)
	}
	existing, err := ctx.GetStub().GetState(key)
	if err != nil {
		return fmt.Errorf("read quote use %q: %w", quoteID, err)
	}
	if existing != nil {
		var used quoteUse
		if err := json.Unmarshal(existing, &used); err == nil {
			return fmt.Errorf(
				"quote %q was already dealt on transfer %q at %d: a quote is single use",
				quoteID, used.TransferID, used.At)
		}
		return fmt.Errorf("quote %q has already been used", quoteID)
	}
	raw, err := json.Marshal(quoteUse{QuoteID: quoteID, TransferID: transferID, At: now})
	if err != nil {
		return fmt.Errorf("encode quote use: %w", err)
	}
	return ctx.GetStub().PutState(key, raw)
}

type quoteUse struct {
	QuoteID    string `json:"quoteID"`
	TransferID string `json:"transferID"`
	At         int64  `json:"at"`
}
