# DRUNIX silently truncates range queries at ten results

Found 17 November 2026 while a conservation check reported a false imbalance.
Reported here in the form it would take upstream.

## Summary

On DRUNIX's SQL state database, `GetStateByPartialCompositeKey` and
`GetStateByRange` return **at most ten results**. There is no error, no
truncation flag, and the iterator reports that it is done. Chaincode that
iterates state gets a partial answer and cannot tell.

`GetQueryResult` has the same defect.

## Cause

`core/ledger/kvledger/txmgmt/statedb/statesqldb/statesqldb.go`:

```go
func (db *versionedDB) GetStateRangeScanIterator(namespace string, startKey string, endKey string) (statedb.ResultsIterator, error) {
	return db.GetStateRangeScanIteratorWithPagination(namespace, startKey, endKey, 10)
}

func (db *versionedDB) ExecuteQuery(namespace, query string) (statedb.ResultsIterator, error) {
	return db.newSqlExecuter(namespace, query, "", 10), nil
}
```

The unpaginated entry point delegates to the paginated one with a hardcoded
page size. Upstream Fabric's LevelDB implementation, in the same position,
passes the sentinel that means no limit:

```go
// core/ledger/kvledger/txmgmt/statedb/stateleveldb/stateleveldb.go
func (vdb *versionedDB) GetStateRangeScanIterator(namespace string, startKey string, endKey string) (statedb.ResultsIterator, error) {
	// pageSize = 0 denotes unlimited page size
	return vdb.GetStateRangeScanIteratorWithPagination(namespace, startKey, endKey, 0)
}
```

`statecouchdb` behaves the same way as `stateleveldb`. The SQL adapter is the
odd one out.

The configured limits are not involved: `totalQueryLimit` is 100000 and
`internalQueryLimit` is 1000 in the deployed `core.yaml`. Neither is consulted
on this path.

## Reproducing it

Any chaincode with more than ten keys under one composite-key prefix. On this
project's settlement chaincode, with sixteen account records on the ledger:

```
$ peer chaincode query -C mychannel -n settlement -c '{"Args":["ListBalances"]}' | jq length
10
$ peer chaincode query -C mychannel -n settlement -c '{"Args":["ListTransfers"]}' | jq length
10
$ peer chaincode query -C mychannel -n settlement -c '{"Args":["ListAttestations"]}' | jq length
10
```

Three unrelated ranges, all exactly ten, stable across repeated calls. An
account absent from `ListBalances` reads back correctly by key:

```
$ peer chaincode query -C mychannel -n settlement \
    -c '{"Args":["GetBalance","escrow:TP-MIDHIT-1794915511","AED"]}'
{"account":"escrow:TP-MIDHIT-1794915511","ccy":"AED","debits":0,"credits":220000,"balance":220000}
```

So the data is there; the range does not return it.

## Why this is worse than a truncated list

A partial list is usually a display bug. Here it is not, because chaincode
**folds** ranges:

- A conservation check that sums every account over a partial read does not
  fail loudly. It answers a different question quietly. In this project it
  reported a deficit of exactly one transfer's two legs on a ledger whose books
  were in fact perfect — and the opposite error, reporting balanced books on a
  partial read, is equally available.
- Worse, a netting window computes net positions by folding its obligations and
  then **settles** them. A truncated read there does not report a wrong total,
  it moves the wrong money and leaves the rest of the window unaccounted for.

Anything that iterates state to compute a total, a balance, a quorum or a
settlement is exposed, and nothing in the API suggests it should be.

## Suggested fix

```diff
 func (db *versionedDB) GetStateRangeScanIterator(namespace string, startKey string, endKey string) (statedb.ResultsIterator, error) {
-	return db.GetStateRangeScanIteratorWithPagination(namespace, startKey, endKey, 10)
+	// pageSize = 0 denotes unlimited page size
+	return db.GetStateRangeScanIteratorWithPagination(namespace, startKey, endKey, 0)
 }
```

with the same change to `ExecuteQuery`, and `newSqlScanner` treating a page size
of zero as unbounded (reading in `internalQueryLimit`-sized batches, as the
other adapters do).

## Workaround, if you cannot patch the platform

Call the paginated API explicitly and follow the bookmark. This project does
that in `chaincode/settlement/settlement/iterate.go`; every range query goes
through one helper so there is a single place to remove it from when the
platform is fixed.

The mock stub in the test suite **reproduces the truncation on purpose**, so
that any code going back to the unpaginated call fails a test rather than a
live ledger. `TestRange_TheUnpaginatedCallTruncates` pins the platform
behaviour; `TestListBalances_FoldsEveryAccountNotTheFirstTen` and
`TestCloseWindow_NetsEveryObligationNotTheFirstTen` guard the two places where
it would have done real damage.

## What this invalidated in this repository

Stated plainly, because the whole point of the conservation check is that it is
believed.

- **Phase B's live conservation claim is not fully supported.** That run
  reported "AED sums to 0, INR sums to 0" across exactly **ten** accounts. Ten
  is the truncation limit, so the ledger may have held more. It cannot now be
  re-checked — the network has been torn down and rebuilt several times since —
  so the claim is withdrawn rather than defended. The in-memory tests of the
  same invariant were never affected.
- **The netting runs are unaffected.** Those folded three or four accounts in a
  single currency, well under the limit, and their arithmetic was additionally
  checked against per-account balances read by key.
- **The Phase C read model is unaffected.** It is built from chaincode events,
  not range queries.
- **The corridor view's window list was exposed** once more than ten windows
  existed, and would have shown a short list without saying so.
