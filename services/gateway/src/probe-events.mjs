// Which peer actually delivers chaincode events — the Lite Peer (7051, stateless,
// endorses) or the Committing Peer (7061, holds the ledger)?
import { promises as fs } from 'node:fs';
import path from 'node:path'; import os from 'node:os'; import crypto from 'node:crypto';
import grpc from '@grpc/grpc-js';
import { connect, hash, signers } from '@hyperledger/fabric-gateway';

const TN = path.join(os.homedir(), '.tributary-pilot/drunix/drunix-network/test-network');
const org1 = path.join(TN, 'organizations/peerOrganizations/org1.example.com');
const first = async (d) => path.join(d, (await fs.readdir(d)).find(f => !f.startsWith('.')));

async function tryPeer(endpoint, hostAlias, label) {
  const tls = await fs.readFile(path.join(org1, 'tlsca/tlsca.org1.example.com-cert.pem'));
  const client = new grpc.Client(endpoint, grpc.credentials.createSsl(tls), {
    'grpc.ssl_target_name_override': hostAlias });
  const msp = path.join(org1, 'users/Admin@org1.example.com/msp');
  const gw = connect({
    client,
    identity: { mspId: 'Org1MSP', credentials: await fs.readFile(await first(path.join(msp,'signcerts'))) },
    signer: signers.newPrivateKeySigner(crypto.createPrivateKey(await fs.readFile(await first(path.join(msp,'keystore'))))),
    hash: hash.sha256,
  });
  try {
    const events = await gw.getNetwork('mychannel').getChaincodeEvents('settlement', { startBlock: BigInt(0) });
    const seen = [];
    const timer = setTimeout(() => events.close(), 12000);
    for await (const e of events) {
      seen.push(e.eventName);
      if (seen.length >= 3) break;
    }
    clearTimeout(timer); events.close();
    console.log(`${label.padEnd(26)} -> ${seen.length ? 'EVENTS: ' + seen.join(', ') : 'no events in 12s'}`);
  } catch (err) {
    console.log(`${label.padEnd(26)} -> ERROR: ${(err.details?.[0]?.message ?? err.message).slice(0,110)}`);
  } finally { gw.close(); client.close(); }
}

await tryPeer('localhost:7051', 'peer0.org1.example.com', 'Lite Peer 7051');
await tryPeer('localhost:7061', 'peer1.org1.example.com', 'Committing Peer 7061');
await tryPeer('localhost:7071', 'peer2.org1.example.com', 'Validation Svc 7071');
process.exit(0);
