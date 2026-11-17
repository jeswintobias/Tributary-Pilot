package settlement

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
)

// DRUNIX's SQL state database truncates an unpaginated range scan at ten rows
// without saying so. These tests exist because the consequence is not a
// crash — it is a conservation check that folds ten accounts out of sixteen
// and reports an answer to a question nobody asked.

// First, pin down the platform behaviour the mock reproduces. If this ever
// stops truncating, the workaround is no longer needed and this test is how
// anybody finds out.
func TestRange_TheUnpaginatedCallTruncates(t *testing.T) {
	l := newLedger(t, 1_700_000_000)
	for i := 0; i < 25; i++ {
		if err := l.seed(fmt.Sprintf("Org%02dMSP", i), aed, 1_000); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	iter, err := l.stub.GetStateByPartialCompositeKey(accountKeyPrefix, []string{})
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	count := 0
	for iter.HasNext() {
		if _, err := iter.Next(); err != nil {
			t.Fatalf("next: %v", err)
		}
		count++
	}
	iter.Close()

	if count != drunixUnpaginatedRangeLimit {
		t.Fatalf("the unpaginated range returned %d records; the mock should "+
			"reproduce DRUNIX's limit of %d", count, drunixUnpaginatedRangeLimit)
	}
}

// And the workaround returns everything.
func TestRange_ThePaginatedHelperReturnsEverything(t *testing.T) {
	l := newLedger(t, 1_700_000_000)
	const orgs = 25
	for i := 0; i < orgs; i++ {
		if err := l.seed(fmt.Sprintf("Org%02dMSP", i), aed, 1_000); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	seen := 0
	if err := forEachInRange(l.as(senderOrg), accountKeyPrefix, []string{},
		func(*queryresult.KV) error { seen++; return nil }); err != nil {
		t.Fatalf("forEachInRange: %v", err)
	}

	// 25 holders plus the issuance account they were all issued against.
	if seen != orgs+1 {
		t.Fatalf("the paginated helper returned %d accounts, want %d", seen, orgs+1)
	}
}

// The helper must page, not just ask for one big page: an implementation that
// caps the page size regardless would otherwise still truncate.
func TestRange_FollowsBookmarksAcrossPages(t *testing.T) {
	l := newLedger(t, 1_700_000_000)
	const orgs = 25
	for i := 0; i < orgs; i++ {
		if err := l.seed(fmt.Sprintf("Org%02dMSP", i), aed, 1_000); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Walk it by hand in pages of three, which is what the helper does when an
	// implementation hands back short pages.
	seen := map[string]bool{}
	bookmark := ""
	for page := 0; page < 100; page++ {
		iter, meta, err := l.stub.GetStateByPartialCompositeKeyWithPagination(
			accountKeyPrefix, []string{}, 3, bookmark)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		n := 0
		for iter.HasNext() {
			kv, err := iter.Next()
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if seen[kv.GetKey()] {
				t.Fatalf("key %q returned twice across pages", kv.GetKey())
			}
			seen[kv.GetKey()] = true
			n++
		}
		iter.Close()
		if n == 0 || meta.GetBookmark() == "" {
			break
		}
		bookmark = meta.GetBookmark()
	}
	if len(seen) != orgs+1 {
		t.Fatalf("paging by hand saw %d accounts, want %d", len(seen), orgs+1)
	}
}

// ---------------------------------------------------------------------------
// The functions that actually matter
// ---------------------------------------------------------------------------

// The conservation check folds every account. On a partial read it does not
// fail loudly — it answers a different question. This is the regression guard
// for the whole invariant.
func TestListBalances_FoldsEveryAccountNotTheFirstTen(t *testing.T) {
	l := newLedger(t, 1_700_000_000)
	const orgs = 25
	for i := 0; i < orgs; i++ {
		if err := l.seed(fmt.Sprintf("Org%02dMSP", i), aed, 1_000); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	accounts, err := l.sc.ListBalances(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListBalances: %v", err)
	}
	if len(accounts) != orgs+1 {
		t.Fatalf("ListBalances returned %d accounts, want %d: a partial fold "+
			"makes the conservation check meaningless", len(accounts), orgs+1)
	}

	// And conservation still holds across all of them.
	l.assertBooksBalance()

	total := int64(0)
	for _, a := range accounts {
		total += a.Balance
	}
	if total != 0 {
		t.Fatalf("accounts sum to %d across %d accounts, want 0", total, len(accounts))
	}
}

func TestListTransfers_ReturnsMoreThanTen(t *testing.T) {
	l := fundedLedger(t)
	const want = 14
	for i := 0; i < want; i++ {
		id := fmt.Sprintf("TP-%02d", i)
		if err := l.create(senderOrg, id, 1_000, aed, 1_000, inr,
			fmt.Sprintf("Q-%02d", i), expiresAt, payoutOrg, payeeRef); err != nil {
			t.Fatalf("CreateTransfer %s: %v", id, err)
		}
	}
	got, err := l.sc.ListTransfers(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListTransfers: %v", err)
	}
	if len(got) != want {
		t.Fatalf("ListTransfers returned %d of %d transfers", len(got), want)
	}
}

func TestListAttestations_ReturnsMoreThanTen(t *testing.T) {
	l := newLedger(t, 1_700_000_000)
	const subjects = 8 // two attestations each, so sixteen records
	for i := 0; i < subjects; i++ {
		if err := l.attest(fmt.Sprintf("sha256:subject-%02d", i)); err != nil {
			t.Fatalf("attest %d: %v", i, err)
		}
	}
	got, err := l.sc.ListAttestations(l.as(senderOrg))
	if err != nil {
		t.Fatalf("ListAttestations: %v", err)
	}
	if len(got) != subjects*2 {
		t.Fatalf("ListAttestations returned %d of %d records", len(got), subjects*2)
	}
}

// Netting folds a window's obligations to compute net positions. A truncated
// read there would not report a wrong total — it would SETTLE one, moving the
// wrong amounts and leaving the rest of the window's obligations unaccounted
// for. This is the most dangerous place the defect could have landed.
func TestCloseWindow_NetsEveryObligationNotTheFirstTen(t *testing.T) {
	l := newLedger(t, baseNow)
	for _, org := range []string{senderOrg, payoutOrg} {
		if err := l.seed(org, "AED", 50_000_000); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seq := l.seqNow()

	// Fourteen obligations, alternating direction, so a truncated read would
	// compute a visibly different net.
	const pairs = 7
	for i := 0; i < pairs; i++ {
		if err := l.submit(senderOrg, fmt.Sprintf("OB-OUT-%02d", i), payoutOrg, "AED", 100_000); err != nil {
			t.Fatalf("submit out %d: %v", i, err)
		}
		if err := l.submit(payoutOrg, fmt.Sprintf("OB-IN-%02d", i), senderOrg, "AED", 90_000); err != nil {
			t.Fatalf("submit in %d: %v", i, err)
		}
	}

	l.at(baseNow + NettingWindowSeconds)
	w, err := l.closeWindow(senderOrg, "AED", seq)
	if err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}

	if w.Obligations != pairs*2 {
		t.Fatalf("the window netted %d obligations, want %d: a truncated read "+
			"would settle the wrong amount, not merely report one", w.Obligations, pairs*2)
	}
	wantGross := uint64(pairs) * (100_000 + 90_000)
	if w.GrossValue != wantGross {
		t.Errorf("GrossValue = %d, want %d", w.GrossValue, wantGross)
	}
	// Seven times ten thousand, one way.
	if w.NetValue != uint64(pairs)*10_000 {
		t.Errorf("NetValue = %d, want %d", w.NetValue, uint64(pairs)*10_000)
	}
	l.assertBooksBalance()
}
