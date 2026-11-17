package settlement

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// A quote the contract will accept, at the exact rate the legs imply.
func goodQuote(l *ledger, id string) Quote {
	return l.quoteFor(id, aed, senderLeg, inr, payoutLeg, l.stub.now+300)
}

// ---------------------------------------------------------------------------
// The rate has to be signed
// ---------------------------------------------------------------------------

func TestQuote_UnsignedRateIsRefused(t *testing.T) {
	l := fundedLedger(t)
	q := goodQuote(l, "Q-UNSIGNED")

	err := l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
		q, "", expiresAt, payoutOrg, payeeRef)
	requireErrContains(t, err, "signature must be between")
}

func TestQuote_ForgedSignatureIsRefused(t *testing.T) {
	l := fundedLedger(t)
	q := goodQuote(l, "Q-FORGED")

	// Signed by a key the chain has never heard of. This is the whole attack:
	// anybody can produce a well-formed signature, only the registered oracle
	// can produce one that verifies.
	attacker, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate attacker key: %v", err)
	}
	sig := l.sign(q, attacker)

	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			q, sig, expiresAt, payoutOrg, payeeRef),
		"is not signed by oracle")
}

func TestQuote_AlteringTheRateAfterSigningIsRefused(t *testing.T) {
	l := fundedLedger(t)
	q := goodQuote(l, "Q-TAMPER")
	sig := l.sign(q, l.oracleKey)

	// A signature over the honest quote, attached to a better one. The payout
	// leg is doubled and the rate doubled with it, so the two still agree with
	// each other — only the signature does not.
	tampered := q
	tampered.RateNum = q.RateNum * 2

	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, 2*payoutLeg, inr,
			tampered, sig, expiresAt, payoutOrg, payeeRef),
		"is not signed by oracle")
}

func TestQuote_UnregisteredOracleIsRefused(t *testing.T) {
	l := fundedLedger(t)
	q := goodQuote(l, "Q-UNKNOWN")
	q.OracleID = "oracle-nobody-registered"
	sig := l.sign(q, l.oracleKey)

	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			q, sig, expiresAt, payoutOrg, payeeRef),
		"is not registered")
}

func TestQuote_RevokedOracleIsRefused(t *testing.T) {
	l := fundedLedger(t)
	if err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.RevokeOracle(c, l.oracleID)
	}); err != nil {
		t.Fatalf("RevokeOracle: %v", err)
	}

	q := goodQuote(l, "Q-AFTER-REVOKE")
	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			q, l.sign(q, l.oracleKey), expiresAt, payoutOrg, payeeRef),
		"was revoked")
}

// ---------------------------------------------------------------------------
// The rate has to be recent
// ---------------------------------------------------------------------------

func TestQuote_ExpiredRateIsRefused(t *testing.T) {
	l := fundedLedger(t)
	q := l.quoteFor("Q-STALE", aed, senderLeg, inr, payoutLeg, l.stub.now-1)

	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			q, l.sign(q, l.oracleKey), expiresAt, payoutOrg, payeeRef),
		"an FX rate is only true for a moment")
}

// ---------------------------------------------------------------------------
// The rate has to be about THIS transfer
// ---------------------------------------------------------------------------

func TestQuote_WrongCurrencyPairIsRefused(t *testing.T) {
	l := fundedLedger(t)
	// A perfectly valid, correctly signed, unexpired quote — for a different
	// corridor. Without this check a signature would be a token that unlocked
	// any transfer.
	q := l.quoteFor("Q-OTHER-PAIR", "USD", senderLeg, "EUR", payoutLeg, l.stub.now+300)

	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			q, l.sign(q, l.oracleKey), expiresAt, payoutOrg, payeeRef),
		"but the transfer is")
}

// This is the test that stops a signed quote being decoration. The quote is
// genuine and current; the legs simply are not what it says.
func TestQuote_LegsThatDoNotMatchTheRateAreRefused(t *testing.T) {
	l := fundedLedger(t)
	q := l.quoteFor("Q-MISMATCH", aed, senderLeg, inr, payoutLeg, l.stub.now+300)
	sig := l.sign(q, l.oracleKey)

	// Same signed quote, but the sender pays half as much for the same payout.
	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg/2, aed, payoutLeg, inr,
			q, sig, expiresAt, payoutOrg, payeeRef),
		"exceeds the")
}

func TestQuote_SlippageIsAllowedUpToTheStatedBound(t *testing.T) {
	l := fundedLedger(t)
	// One percent of tolerance, and a payout half a percent away from the
	// quoted rate: inside the bound.
	q := l.quoteFor("Q-SLIP", aed, senderLeg, inr, payoutLeg, l.stub.now+300)
	q.MaxSlippageBps = 100
	within := payoutLeg - payoutLeg/200

	if err := l.createWith(senderOrg, "TP-OK", senderLeg, aed, within, inr,
		q, l.sign(q, l.oracleKey), expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("a payout inside the stated tolerance should be accepted: %v", err)
	}

	// Five percent away, with the same one percent tolerance: outside.
	q2 := l.quoteFor("Q-SLIP-2", aed, senderLeg, inr, payoutLeg, l.stub.now+300)
	q2.MaxSlippageBps = 100
	beyond := payoutLeg - payoutLeg/20

	requireErrContains(t,
		l.createWith(senderOrg, "TP-BAD", senderLeg, aed, beyond, inr,
			q2, l.sign(q2, l.oracleKey), expiresAt, payoutOrg, payeeRef),
		"exceeds the")
}

func TestQuote_SlippageBeyondOneHundredPercentIsRefused(t *testing.T) {
	l := fundedLedger(t)
	q := goodQuote(l, "Q-ABSURD")
	q.MaxSlippageBps = 10_001

	requireErrContains(t,
		l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
			q, l.sign(q, l.oracleKey), expiresAt, payoutOrg, payeeRef),
		"exceeds 100%")
}

// ---------------------------------------------------------------------------
// A quote is dealt once
// ---------------------------------------------------------------------------

func TestCreateTransfer_RefusesAReusedQuote(t *testing.T) {
	l := fundedLedger(t)
	q := goodQuote(l, "Q-ONCE")
	sig := l.sign(q, l.oracleKey)

	if err := l.createWith(senderOrg, "TP-1", senderLeg, aed, payoutLeg, inr,
		q, sig, expiresAt, payoutOrg, payeeRef); err != nil {
		t.Fatalf("first use of a quote: %v", err)
	}
	requireErrContains(t,
		l.createWith(senderOrg, "TP-2", senderLeg, aed, payoutLeg, inr,
			q, sig, expiresAt, payoutOrg, payeeRef),
		"a quote is single use")
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestRegisterOracle_OnlyAnApprovedOrgMay(t *testing.T) {
	l := fundedLedger(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	err = l.invoke(senderOrg, func(c contractCtx) error {
		return l.sc.RegisterOracle(c, "oracle-rogue", publicKeyPEM(t, key))
	})
	requireErrContains(t, err, "may not register an oracle key")
}

func TestRegisterOracle_RefusesSilentKeyReplacement(t *testing.T) {
	l := fundedLedger(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	requireErrContains(t, l.registerOracle(l.oracleID, key), "already registered")
}

func TestRegisterOracle_RefusesAnUnusableKey(t *testing.T) {
	l := fundedLedger(t)
	err := l.invoke(attestorOrg, func(c contractCtx) error {
		return l.sc.RegisterOracle(c, "oracle-junk", "-----BEGIN PUBLIC KEY-----\nnope\n-----END PUBLIC KEY-----")
	})
	requireErrContains(t, err, "not a usable ECDSA public key")
}

// ---------------------------------------------------------------------------
// Canonical bytes
// ---------------------------------------------------------------------------

// Without length prefixes, two different quotes can serialise identically and
// a signature over one verifies the other. This checks the encoding separates
// adjacent fields.
func TestQuoteBytes_FieldsCannotBleedIntoEachOther(t *testing.T) {
	a := Quote{QuoteID: "AB", OracleID: "C", BaseCcy: "AED", QuoteCcy: "INR",
		RateNum: 1, RateDen: 1, ExpiresAt: 1, MaxSlippageBps: 0}
	b := Quote{QuoteID: "A", OracleID: "BC", BaseCcy: "AED", QuoteCcy: "INR",
		RateNum: 1, RateDen: 1, ExpiresAt: 1, MaxSlippageBps: 0}

	if string(QuoteBytes(a)) == string(QuoteBytes(b)) {
		t.Fatal("two different quotes serialise to the same bytes; a signature over one would verify the other")
	}
}

func TestQuoteBytes_IsStableForTheSameQuote(t *testing.T) {
	q := Quote{QuoteID: "Q", OracleID: "O", BaseCcy: "AED", QuoteCcy: "INR",
		RateNum: 227431, RateDen: 10000, ExpiresAt: 1700000000, MaxSlippageBps: 50}
	first := string(QuoteBytes(q))
	for i := 0; i < 50; i++ {
		if string(QuoteBytes(q)) != first {
			t.Fatal("QuoteBytes is not deterministic, so peers would disagree on every signature")
		}
	}
}

// ---------------------------------------------------------------------------
// Arithmetic
// ---------------------------------------------------------------------------

func TestMulDiv(t *testing.T) {
	cases := []struct {
		name    string
		a, b, c uint64
		want    uint64
		wantErr bool
	}{
		{"ordinary", 220000, 227431, 10000, 5003482, false},
		{"exact", 100, 3, 2, 150, false},
		{"truncates toward zero", 10, 1, 3, 3, false},
		{"division by zero", 1, 1, 0, 0, true},
		// a*b overflows uint64 here. The naive expression would wrap and
		// produce a rate check that passes anything.
		{"product beyond 64 bits still divides correctly", math.MaxUint64, 2, 4, math.MaxUint64 / 2, false},
		{"quotient too large", math.MaxUint64, math.MaxUint64, 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mulDiv(tc.a, tc.b, tc.c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mulDiv(%d, %d, %d) = %d, wanted an error", tc.a, tc.b, tc.c, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mulDiv(%d, %d, %d): %v", tc.a, tc.b, tc.c, err)
			}
			if got != tc.want {
				t.Errorf("mulDiv(%d, %d, %d) = %d, want %d", tc.a, tc.b, tc.c, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The proof hash covers the rate
// ---------------------------------------------------------------------------

// Prepare anchors a hash over the transfer's economic terms and Commit
// re-checks it. The rate is one of those terms, so altering the recorded rate
// between the two phases must break the hash.
func TestProofHash_CoversTheRate(t *testing.T) {
	l := fundedLedger(t)
	base := Transfer{
		TransferID: "TP-1", SenderOrg: senderOrg, SenderLegAmount: senderLeg, SenderCcy: aed,
		PayoutOrg: payoutOrg, PayoutLegAmount: payoutLeg, PayoutCcy: inr,
		PayoutPayeeRef: payeeRef, QuoteID: "Q", ExpiresAt: expiresAt,
		Quote: goodQuote(l, "Q"),
	}
	altered := base
	altered.Quote.RateNum = base.Quote.RateNum + 1

	if proofHash(base) == proofHash(altered) {
		t.Fatal("the proof hash does not cover the rate, so terms could be changed between prepare and commit")
	}
}

// ---------------------------------------------------------------------------
// Agreement with the oracle service
// ---------------------------------------------------------------------------

// The quote encoding exists twice: here in Go, and in services/oracle in
// JavaScript. They have to produce identical bytes or every quote the service
// signs is rejected on chain, with an error that says the signature is wrong
// rather than that the encodings disagree.
//
// Nothing but care keeps two implementations of a wire format in step, which
// is not enough for something that fails this confusingly. The service writes
// this vector with `npm run vector`; this test re-derives the bytes with the
// Go implementation and verifies the service's signature with the Go verifier.
// Either side drifting fails here.
func TestQuote_MatchesTheOracleServiceEncoding(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "quote-vector.json"))
	if err != nil {
		t.Fatalf("read the cross-language vector: %v "+
			"(regenerate it with: cd services/oracle && npm run vector)", err)
	}

	var vector struct {
		Quote           Quote  `json:"quote"`
		CanonicalBase64 string `json:"canonicalBase64"`
		PublicKeyPEM    string `json:"publicKeyPEM"`
		SignatureBase64 string `json:"signatureBase64"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("decode the vector: %v", err)
	}

	wantBytes, err := base64.StdEncoding.DecodeString(vector.CanonicalBase64)
	if err != nil {
		t.Fatalf("decode canonicalBase64: %v", err)
	}
	gotBytes := QuoteBytes(vector.Quote)
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("the two implementations of the quote encoding disagree\n"+
			"  Go      : %q\n  service : %q", gotBytes, wantBytes)
	}

	// And the signature the service produced must verify with the Go verifier,
	// which is what the chaincode actually runs.
	pub, err := parsePublicKey(vector.PublicKeyPEM)
	if err != nil {
		t.Fatalf("parse the vector's public key: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(vector.SignatureBase64)
	if err != nil {
		t.Fatalf("decode the signature: %v", err)
	}
	digest := sha256.Sum256(gotBytes)
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatal("a signature produced by the oracle service does not verify in the chaincode")
	}
}
