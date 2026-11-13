# The settlement pipeline

```
ledger ──► projector ──► read model + outbox ──► payout worker ──► payouts / dead letters
                │                                      │
                └── checkpoint                         └── SIMULATED rail
```

The chaincode demos prove the ledger is right. They say nothing about the
layer around it, and that layer is where a different class of failure lives:
an event missed while a process was down, an event applied twice after a
restart, a settled transfer that is never paid, one broken payout blocking
every payout behind it.

This is how each of those is dealt with, and what is still open.

## 1. Why there is a read model at all

Every endpoint in the gateway reads the ledger directly: one query per request,
answered by a peer. That is correct and it does not scale. A dashboard
refreshing every few seconds becomes sustained load on peers whose actual job
is endorsing transactions, and a chaincode query can only ever return what a
peer will compute on demand — never "how many transfers settled last week",
because nothing on the ledger is indexed that way.

So reads that do not need to be authoritative come from a projection built by
replaying the chain. It holds the same facts. What it does not hold is anything
newer than the last event applied, and **that gap is reported rather than
hidden**: `/api/pipeline` returns how many blocks behind the projection is, and
the view says so. A dashboard presenting stale figures as live is worse than
one that admits the lag.

The ledger stays the authority. Anything that must be true at the moment it is
read — a balance before a transfer, the state before an action — still goes to
a peer.

## 2. The checkpoint, and what it points at

The SDK's `getChaincodeEvents` starts from the next block to be committed
unless told otherwise. A projector that does not pass `startBlock` therefore
loses everything that happened while it was down, silently, leaving no gap
anyone can notice afterwards. The first version of the gateway's event stream
did exactly this.

The checkpoint records the block to **resume from**, and that is the block of
the last applied event — not the one after it. A block can hold several events,
and advancing past the block after applying its first event would lose the rest
if the process died in between. Resuming from the same block re-delivers all of
them.

So **delivery is at-least-once and application is exactly-once**. That is the
only combination available, because Fabric can be asked to resume from a block
and not from an event. The `applied_events` table, keyed by block, transaction
and event name, is what turns the second delivery into a no-op.

## 3. One transaction, or the dual-write problem

When an event arrives the projector has three things to do: update the read
model, record that the event was applied, and enqueue any work it implies. If
those live in different stores, every crash lands in one of three bad states:

| Crash point | Result |
|---|---|
| checkpoint advances, read model does not | the event is lost for ever |
| read model updates, checkpoint does not | the event is applied twice |
| both update, enqueue does not | a settled transfer is never paid |

No amount of retrying or compensating fixes this at the application level; it
only moves the window in which it bites. Putting all three in one transactional
store removes it instead — SQLite commits them together or not at all.

That is why the queue is a table in the same database as the read model, and
why it is a table rather than a broker. A broker would add an operational
dependency and would not make any of the above easier. The interface in
`queue.js` is four functions wide, so moving to one later is a change to that
file.

`a failure part-way through an event leaves no trace of it at all` is the test:
it forces a failure after the transfer row is written and asserts the row, the
job, the applied-events record and the checkpoint are all unchanged.

## 4. Leases, not locks

A claimed job is leased until a deadline. A worker that crashes holds nothing —
its lease expires and another worker picks the job up. A lock would need the
crashed process to come back and release it, which is the one thing a crashed
process cannot be relied on to do.

The cost is that a job can run twice: once by a worker that stalled past its
deadline, once by its replacement. There is no setting that removes this, so
paying twice has to be impossible for other reasons:

- the `payouts` row is keyed by the job's dedupe key, derived from the transfer
  ID, so a second success overwrites the same row rather than adding one
- the rail reference is **derived from the transfer, not generated**, so a
  retry presents the same idempotency key the first attempt did. A random
  reference would make a retry indistinguishable from a second payment, both
  here and at the rail

## 5. Dead letters

A job that keeps failing is retried with exponential backoff — 1s, 2s, 4s, 8s,
16s, capped at a minute — and after five attempts it is moved to
`dead_letters`.

The point is not the retrying. It is that **one unprocessable payout must not
block every payout behind it**, which is what a queue without a dead-letter
path does. `a poison job does not block the work behind it` settles that: a
transfer whose beneficiary the rail always rejects dead-letters while a healthy
transfer queued behind it is paid.

Rows are moved, not deleted. A payment that did not happen is something
somebody has to look at.

An error the rail marks non-retryable — no beneficiary, a malformed amount —
skips the retries and dead-letters on the first attempt. Waiting cannot conjure
a beneficiary.

## 6. Money does not become a float

The chaincode marshals amounts as JSON numbers, because that is what Go's
`encoding/json` does with a `uint64`. `JSON.parse` turns those into doubles,
which hold integers exactly only to 2^53 − 1:

```
JSON.parse('{"a":9007199254740993}').a   ->   9007199254740992
```

No error, one fil gone. The gateway was already careful about this on the way
**in** — request amounts are parsed with `BigInt` — and was undoing it on the
way **out**. This was recorded as an open gap when it was found; the projector
is where it is fixed.

`parsePayload` uses a reviver that keeps the original source text of every
integer, so an amount never passes through a double at all. It is stored as
`TEXT`, handed to the rail as text, and compared with `BigInt`. Two tests cover
it, both asserting against 9007199254740993 specifically, and one of them
asserts that the naive parse would have differed — so the guard cannot quietly
stop guarding anything.

Integers are kept as text wholesale rather than by field name: deciding by name
would reintroduce the bug for the next field somebody adds. Callers that want a
number for a block or a sequence convert explicitly, which is the point at
which the precision question becomes visible.

## 7. What is deliberately not solved

- **The worker polls.** SQLite has no push. The poll interval is the latency
  floor, stated rather than hidden. At the rate a corridor produces payouts it
  costs nothing; at a much higher rate it would want a broker.
- **Backoff is not jittered.** With one worker a deterministic backoff makes
  tests reproducible. With several, they would retry in lockstep and want
  jitter.
- **One process per role, one machine.** The projector and the worker are
  separate processes but there is no leader election, so running two projectors
  against one store would have them both apply the same events — correctly,
  because of the dedupe table, but pointlessly. Several workers are fine: that
  is what the leases are for.
- **The rail is simulated, and labelled so everywhere.** A real one needs a
  licensed counterparty. What the simulation is actually for is making failures
  real: `PAYOUT_FAIL_REFS` and `PAYOUT_FLAKY_REFS` fail deterministically, so a
  test asserting "this reaches the dead-letter table and that recovers on
  retry" is a test rather than a coin toss.
- **Nothing re-drives a dead letter.** They accumulate for a human. An operator
  action to requeue one is obvious work and is not done.
- **The projection can be rebuilt but is not verified against the ledger.**
  Deleting the store and restarting replays the chain from block 0, which is
  the recovery path. Nothing periodically checks that the projection still
  agrees with the ledger, which is what would catch a projection bug that only
  bites on one event shape.

## 8. Running it

```bash
cd services/projector && npm install
npm run projector     # replay from the checkpoint and follow
npm run worker        # drain the payout queue
npm run show          # print the read model
npm test              # 27 tests
```

`PROJECTOR_IDLE_EXIT_MS` and `WORKER_IDLE_EXIT_MS` make each process stop once
it has caught up, which is how `network/demo-pipeline.sh` drives them and gets
an exit code.

The whole pipeline, against the live network:

```bash
./network/demo-pipeline.sh
```

It settles two transfers — one the simulated rail accepts, one it always
rejects — then runs the projector, restarts it to prove the replay changes
nothing, runs the worker, and checks that the good payout went through while
the bad one dead-lettered.
