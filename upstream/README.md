# Upstream contribution

One patch for [`npci/drunix`](https://github.com/npci/drunix), found while
building this project.

| File | What it is |
|---|---|
| `0001-statesqldb-unlimited-range-scan.patch` | The fix, against upstream commit `8090e0c` |
| `ISSUE.md` | The report, written as it would be filed |

## What it fixes

On the SQL state database, `GetStateByPartialCompositeKey` and
`GetStateByRange` return **at most ten results**. No error, no truncation flag;
the iterator simply says it is done. `GetQueryResult` has the same defect.

Chaincode that folds a range — a conservation check, a netting calculation, a
quorum count — gets a partial answer and cannot tell. In this project it made a
conservation check report a deficit of exactly one transfer's two legs on a
ledger whose books were perfect.

## Why the obvious one-line fix is wrong

Upstream Fabric's other adapters pass `0` to mean "no limit":

```go
// stateleveldb
// pageSize = 0 denotes unlimited page size
return vdb.GetStateRangeScanIteratorWithPagination(namespace, startKey, endKey, 0)
```

Changing DRUNIX's `10` to `0` on its own makes things **worse**, not better,
because the SQL scanner gates on the page size directly:

```go
func (s *SqlScanner) Next() (*statedb.VersionedKV, error) {
	if s.Cursor >= s.PageSize {   // with PageSize 0, this is true immediately
		return nil, nil
	}
```

A page size of zero would stop the scan before its first row, turning a
ten-result truncation into a zero-result one. So the patch changes three
things together: both call sites pass the sentinel, and both iterators learn
what it means.

## Applying it

```bash
git clone https://github.com/npci/drunix && cd drunix
git checkout 8090e0c
git apply /path/to/0001-statesqldb-unlimited-range-scan.patch
go build ./core/ledger/kvledger/txmgmt/statedb/statesqldb/
```

## What has and has not been verified

**Verified.** The defect itself, on a running DRUNIX network — three unrelated
range queries each returning exactly ten rows while an eleventh key read back
fine by key. The cause, by reading the adapter against `stateleveldb` and
`statecouchdb`. That the patch compiles and passes `go vet`.

**Not verified.** The patch has not been run on a live network. Doing that
means rebuilding DRUNIX's peer images, and DRUNIX publishes amd64 only — on
the Apple-silicon machine this was built on, that is an emulated build that was
not attempted. A maintainer with an amd64 host can test it in the time it takes
to run the reproduction below.

This project works around the defect instead, in
`chaincode/settlement/settlement/iterate.go`: every range query goes through
one paginated helper, so there is a single place to delete when the platform is
fixed.

## Reproducing

Any chaincode with more than ten keys under one composite-key prefix:

```bash
peer chaincode query -C mychannel -n <cc> -c '{"Args":["ListBalances"]}' | jq length
# 10, on a ledger holding 16
```

The project's own test suite reproduces it in-process: its mock stub truncates
at ten **on purpose**, so that code regressing to the unpaginated call fails a
test rather than a production ledger. See
`TestRange_TheUnpaginatedCallTruncates`.
