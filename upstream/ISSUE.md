# Range queries silently return at most 10 results on the SQL state database

## Summary

With `statesqldb`, `GetStateByPartialCompositeKey`, `GetStateByRange` and
`GetQueryResult` return at most **ten** results. There is no error and no
truncation flag — the iterator reports that it is exhausted. Chaincode that
iterates state to compute a total gets a partial answer and cannot detect it.

Found on commit `8090e0c`.

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

The unpaginated entry points delegate to the paginated ones with a hardcoded
page size. The other adapters pass the sentinel that means no limit:

```go
// stateleveldb/stateleveldb.go
// pageSize = 0 denotes unlimited page size
return vdb.GetStateRangeScanIteratorWithPagination(namespace, startKey, endKey, 0)
```

`statecouchdb` behaves the same as `stateleveldb`. `statesqldb` is the odd one
out.

The configured limits are not involved. `totalQueryLimit` (100000) and
`internalQueryLimit` (1000) are not consulted on this path.

## Reproducing

Deploy any chaincode that ranges over a composite key, and put more than ten
keys under one prefix.

```
$ peer chaincode query -C mychannel -n settlement -c '{"Args":["ListBalances"]}' | jq length
10
$ peer chaincode query -C mychannel -n settlement -c '{"Args":["ListTransfers"]}' | jq length
10
$ peer chaincode query -C mychannel -n settlement -c '{"Args":["ListAttestations"]}' | jq length
10
```

Three unrelated ranges, all exactly ten, stable across repeated calls. A record
absent from the range reads back correctly by key:

```
$ peer chaincode query -C mychannel -n settlement \
    -c '{"Args":["GetBalance","escrow:TP-MIDHIT-1794915511","AED"]}'
{"account":"escrow:TP-MIDHIT-1794915511","ccy":"AED","debits":0,"credits":220000,"balance":220000}
```

## Impact

A truncated list is usually a display bug. Here it is not, because chaincode
folds ranges.

- A conservation check that sums every account over a partial read does not
  fail loudly; it answers a different question quietly. In the project that
  found this, it reported a deficit of exactly one transfer's two legs on a
  ledger whose books were correct. The opposite error — reporting balanced
  books on a partial read — is equally available.
- A netting calculation folds a window's obligations and then **settles** them.
  A truncated read there does not report a wrong total, it moves the wrong
  money and leaves the rest of the window unaccounted for.

Anything that iterates state to compute a total, a balance, a quorum or a
settlement is exposed, and nothing in the API suggests it should be.

## Fix

Attached as a patch. Note that changing `10` to `0` alone makes this worse:
both iterators gate on

```go
if s.Cursor >= s.PageSize {
	return nil, nil
}
```

so a page size of zero stops the scan before its first row. The patch changes
the two call sites and both iterators together:

```go
if s.PageSize > 0 && s.Cursor >= s.PageSize {
```

Compiles and passes `go vet`. Not yet run against a live network — DRUNIX
publishes amd64 images only and this was found on Apple silicon, where
rebuilding the peer image is an emulated build.

## Unrelated observation

While reading this code: `sqlSchema.Next` fetches one row per call with
`Limit(1).Offset(cursor)`, so iterating N rows costs N queries each scanning an
increasing offset. That is O(n²) over a large range and is a separate concern
from this bug — mentioned only because the fix above makes large ranges
reachable where previously they stopped at ten.
