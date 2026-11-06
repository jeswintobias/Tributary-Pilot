# Gateway and corridor view

A Node service that talks to the **live DRUNIX network** through the Fabric
Gateway SDK, and a single page that drives it.

Nothing on the page is mocked. Every figure is read from the ledger, every
action is a real endorsed transaction, and every refusal is the chaincode's own
error message passed through verbatim.

## Run it

The network must be up with the settlement chaincode deployed and PlatformOrg
added:

```bash
./network/up.sh
CC_VERSION=3.0 ./network/deploy-cc.sh settlement
./network/add-platform-org.sh
```

Then:

```bash
cd services/gateway
npm install
npm run check               # proves the SDK can reach DRUNIX before anything else
GATEWAY_DEV=1 npm start     # http://127.0.0.1:7700
```

`GATEWAY_DEV=1` is the demo posture: `SeedBalance` works and the page collects
its own tokens, so the organisation switcher is one click. Without it the
service starts in production posture — `SeedBalance` returns 403 and you sign
in with a token.

### Access control

- It binds to **loopback only**. `GATEWAY_HOST` overrides that, and the startup
  banner warns you when it does. There is no TLS here, so anything other than
  loopback needs a proxy in front.
- Every endpoint but the public org roster needs `Authorization: Bearer <token>`.
- **A token maps to exactly one organisation.** The caller cannot name an
  identity; the token decides. Before this, the request body carried
  `as: "Org2MSP"` and the service believed it.
- Tokens are generated with `crypto.randomBytes` on first boot and written to
  `.tokens.json` with mode 0600. There is no default credential. Override with
  `GATEWAY_TOKENS` or `GATEWAY_TOKEN_FILE`.
- 60 requests per 10 seconds per token; `GATEWAY_RATE_LIMIT` changes it.

In production each organisation runs its own gateway holding only its own key.
One process holding all three is a single-operator convenience for this demo,
and it is recorded as still-open item 2 in `docs/THREAT_MODEL.md` T7.

`npm run check` is worth running first. It connects as all three organisations
and reads the ledger; if it fails, the UI will fail the same way and the message
will be clearer here.

## What to try

1. **Capture a transfer.** Press *Ravi example*, then *Capture transfer*. It
   appears as `CAPTURED`.
2. **Try to skip a step.** As SenderOrg, there is no Prepare button on a
   `CAPTURED` transfer — the state machine does not allow it. Switch to
   PayoutBankOrg and press *Accept* first.
3. **Act as the regulator.** Switch to **PlatformOrg** and press any action.
   It is a full channel member, so the call reaches the chaincode — and the
   contract refuses it:

   ```
   caller Org3MSP is not a party to transfer "…" (parties: Org1MSP, Org2MSP)
   ```

   The same identity reads the whole ledger without trouble. That is what a
   regulator observer should be able to do, and it is threat T2.1 from the
   threat model, visible rather than asserted.
4. **Watch the events.** Each committed state change arrives on the event
   stream with its block number.

## How it is wired

| | |
|---|---|
| `src/fabric.js` | Three Gateway connections, one per organisation. PlatformOrg has no peer of its own, so it connects through SenderOrg's Lite Peer with its own signing material |
| `src/server.js` | HTTP API and static host. A chaincode rejection returns **422** with the contract's message, because the ledger understood the call and refused it — that is a result, not a server fault |
| `src/auth.js` | Bearer tokens, one organisation each, plus rate limiting |
| `src/validate.js` | Request bounds. Amounts are parsed with `BigInt`, never as doubles — a uint64 of minor units exceeds what a double holds exactly |
| `src/check.js` | Connectivity probe |
| `src/probe-events.mjs` | Shows which peer can actually deliver events |
| `public/index.html` | The page. No build step, no framework |

Two details that cost time and are worth knowing:

- **Endorsers are named explicitly** (`Org1MSP`, `Org2MSP`) rather than
  discovered. The chaincode policy is `AND(Org1MSP.peer, Org2MSP.peer)`, and
  DRUNIX's split Lite/Committing peer topology does not advertise endorsers the
  way stock Fabric's discovery expects.
- **Peer 0 is the Lite Peer**, which endorses and holds the chaincode. Peer 1 is
  the Committing Peer and has no chaincode installed, so pointing a client at it
  fails with a confusing "chaincode is not installed".

## Deliberate deviations

The Phase 1 plan calls for TypeScript services and a Next.js front end. This is
plain JavaScript with no build step, because a judge should be able to run
`npm install && npm start` and see a working corridor. The framework version is
Phase 1 proper; this is the thinnest honest thing that proves the ledger works
end to end.

Action buttons are **not** filtered by identity. Acting as the wrong
organisation and watching the contract refuse is the clearest demonstration of
the access control, so the buttons offer whatever the state machine allows next
and let the chaincode have the final word.
