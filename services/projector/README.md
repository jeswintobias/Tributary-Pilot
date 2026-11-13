# Projector, queue and payout worker

Two processes over one SQLite store.

```
ledger ──► projector ──► read model + outbox ──► payout worker ──► payouts / dead letters
                │                                      │
                └── checkpoint                         └── SIMULATED rail
```

```bash
npm install
npm run projector   # replay from the checkpoint and follow the chain
npm run worker      # drain the payout queue
npm run show        # print the read model
npm test            # 27 tests
```

Both processes run until stopped. `PROJECTOR_IDLE_EXIT_MS` and
`WORKER_IDLE_EXIT_MS` make them exit once caught up, which is how
`network/demo-pipeline.sh` drives them and gets an exit code.

## The four properties this exists to have

| Property | How |
|---|---|
| A restart loses no event | the stream resumes from a stored checkpoint, not from "now" |
| A restart duplicates no event | `applied_events` makes a second application a no-op |
| A crash cannot split a write | the read model, the checkpoint and the queue are one transaction |
| One bad payout blocks nothing | retries with backoff, then the job moves to `dead_letters` |

Why it is one SQLite file, why the queue is a table, why jobs are leased rather
than locked, and what is deliberately not solved:
[docs/PIPELINE.md](../../docs/PIPELINE.md).

## The rail is simulated

`src/payout-rail.js` stands in for UPI or e-rupee. It reaches no real payment
network, and it is labelled as simulated everywhere it appears.

Its job is to make the failure paths real. It fails deterministically rather
than randomly, so a test asserting "this one dead-letters and that one recovers
on retry" is a test and not a coin toss:

| Variable | Effect |
|---|---|
| `PAYOUT_FAIL_REFS` | payee references that fail on every attempt |
| `PAYOUT_FLAKY_REFS` | payee references that fail until `PAYOUT_FLAKY_ATTEMPTS` |
| `PAYOUT_LATENCY_MS` | artificial delay per call |

## Configuration

| Variable | Default |
|---|---|
| `PROJECTOR_DB` | `data/tributary.db` |
| `PROJECTOR_MSP` | `Org1MSP` |
| `CHANNEL_NAME` | `mychannel` |
| `CC_NAME` | `settlement` |

The projector connects to the **Committing Peer** (port 7061/9061), not the
Lite Peer. DRUNIX splits the peer in two and only the Committing Peer holds a
ledger, so only it can replay blocks from a checkpoint.

To rebuild the read model from scratch, delete `data/` and run the projector
again: it replays the chain from block 0.
