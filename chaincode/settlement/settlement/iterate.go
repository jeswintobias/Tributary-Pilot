package settlement

import (
	"fmt"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
)

// Range queries that actually return everything.
//
// THE BUG THIS WORKS AROUND
//
// On DRUNIX's SQL state database, GetStateByPartialCompositeKey silently
// returns at most TEN results. Not an error, not a truncation flag — ten rows
// and then the iterator says it is done.
//
// The cause is in the state database adapter. Upstream Fabric's LevelDB
// implementation reads:
//
//	func (vdb *versionedDB) GetStateRangeScanIterator(...) (...) {
//		// pageSize = 0 denotes unlimited page size
//		return vdb.GetStateRangeScanIteratorWithPagination(ns, start, end, 0)
//	}
//
// and DRUNIX's SQL implementation reads:
//
//	func (db *versionedDB) GetStateRangeScanIterator(...) (...) {
//		return db.GetStateRangeScanIteratorWithPagination(ns, start, end, 10)
//	}
//
// — the unpaginated entry point delegating to the paginated one with a
// hardcoded page size instead of the sentinel that means "no limit".
// ExecuteQuery has the same defect.
//
// WHY IT MATTERS HERE MORE THAN MOST PLACES
//
// This contract's conservation check folds every account and asserts the total
// is zero. Reading ten accounts out of sixteen does not make that check fail
// loudly — it makes it answer a different question, quietly. It reported a
// false imbalance on a ledger whose books were in fact perfect, which is the
// better of the two ways it could have gone wrong: the other is reporting
// balanced books on a partial read.
//
// THE WORKAROUND
//
// Ask for a page size explicitly. The paginated entry point honours what it is
// given, so a large page returns everything in one call, and the loop below
// follows the bookmark in case an implementation chooses to cap it anyway.
//
// This is a workaround and not a fix. The fix belongs upstream, where the
// sentinel should be 0; until then every chaincode on DRUNIX that iterates
// state is exposed to this, whether or not its authors know.

// rangePageSize is what we ask for per page. Large enough that one call
// normally suffices, bounded so that a pathological range cannot be pulled
// into memory all at once.
const rangePageSize int32 = 1000

// maxRangePages bounds the loop. A range big enough to exceed this is one no
// query-only function should be folding anyway, and an unbounded loop against
// a misbehaving bookmark would hang the peer rather than fail.
const maxRangePages = 1000

// forEachInRange calls visit for every key under a partial composite key.
//
// Query-only. A range query inside a writing transaction is exposed to phantom
// reads, which is why nothing that changes state calls this — except
// CloseWindow, whose range is frozen by time before it is read.
func forEachInRange(
	ctx contractapi.TransactionContextInterface,
	objectType string,
	attributes []string,
	visit func(*queryresult.KV) error,
) error {
	bookmark := ""
	seen := 0
	for page := 0; page < maxRangePages; page++ {
		iter, metadata, err := ctx.GetStub().GetStateByPartialCompositeKeyWithPagination(
			objectType, attributes, rangePageSize, bookmark)
		if err != nil {
			return fmt.Errorf("range over %s: %w", objectType, err)
		}

		count := 0
		for iter.HasNext() {
			kv, err := iter.Next()
			if err != nil {
				iter.Close()
				return fmt.Errorf("read next %s: %w", objectType, err)
			}
			if err := visit(kv); err != nil {
				iter.Close()
				return err
			}
			count++
			seen++
		}
		iter.Close()

		// A page that returned nothing is the end, whatever the bookmark says.
		if count == 0 {
			return nil
		}
		next := ""
		if metadata != nil {
			next = metadata.GetBookmark()
		}
		// No bookmark, or one that has not moved, means there is no next page.
		// Trusting a repeated bookmark would loop for ever.
		if next == "" || next == bookmark {
			return nil
		}
		bookmark = next
	}
	return fmt.Errorf(
		"range over %s did not terminate after %d pages (%d records): refusing to read further",
		objectType, maxRangePages, seen)
}
