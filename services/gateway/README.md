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
npm run check     # proves the SDK can reach DRUNIX before anything else
npm start         # http://localhost:7700
```

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
| `src/check.js` | Connectivity probe |
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
