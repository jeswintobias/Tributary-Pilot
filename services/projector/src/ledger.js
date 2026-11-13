// The projector's connection to the ledger: one identity, one job, events only.
//
// This does not reuse the gateway's connection module on purpose. That one
// holds three identities so the UI can act as any of them and submit
// transactions; a projector needs exactly one read-only identity and an event
// stream, and giving it submit rights it never uses would be a worse answer
// than eighty lines of connection code.
//
// It connects to the COMMITTING PEER, not the Lite Peer. DRUNIX splits the
// peer in two: the Lite Peer endorses and is stateless, the Committing Peer
// validates and commits and is the one that holds a ledger. Only a peer with a
// ledger can replay blocks, so only the Committing Peer can serve an event
// stream that starts from a checkpoint — a Lite Peer delivers nothing.

import { promises as fs } from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import crypto from 'node:crypto';
import grpc from '@grpc/grpc-js';
import { connect, hash, signers } from '@hyperledger/fabric-gateway';

const TRIBUTARY_HOME = process.env.TRIBUTARY_HOME ?? path.join(os.homedir(), '.tributary-pilot');
const TEST_NETWORK = process.env.TEST_NETWORK_DIR
  ?? path.join(TRIBUTARY_HOME, 'drunix', 'drunix-network', 'test-network');

export const CHANNEL = process.env.CHANNEL_NAME ?? 'mychannel';
export const CHAINCODE = process.env.CC_NAME ?? 'settlement';
export const PROJECTOR_MSP = process.env.PROJECTOR_MSP ?? 'Org1MSP';

const peerOrg = (n) => path.join(TEST_NETWORK, 'organizations', 'peerOrganizations', `org${n}.example.com`);

const ORGS = {
  Org1MSP: {
    label: 'SenderOrg',
    tlsCert: path.join(peerOrg(1), 'tlsca', 'tlsca.org1.example.com-cert.pem'),
    mspDir: path.join(peerOrg(1), 'users', 'Admin@org1.example.com', 'msp'),
    endpoint: 'localhost:7061',
    hostAlias: 'peer1.org1.example.com',
  },
  Org2MSP: {
    label: 'PayoutBankOrg',
    tlsCert: path.join(peerOrg(2), 'tlsca', 'tlsca.org2.example.com-cert.pem'),
    mspDir: path.join(peerOrg(2), 'users', 'Admin@org2.example.com', 'msp'),
    endpoint: 'localhost:9061',
    hostAlias: 'peer1.org2.example.com',
  },
};

async function firstFileIn(dir) {
  const entries = await fs.readdir(dir);
  const file = entries.find((e) => !e.startsWith('.'));
  if (!file) throw new Error(`no files in ${dir}`);
  return path.join(dir, file);
}

let connection = null;

export async function connectLedger(mspId = PROJECTOR_MSP) {
  if (connection) return connection;
  const org = ORGS[mspId];
  if (!org) throw new Error(`no connection details for ${mspId}`);

  const tls = await fs.readFile(org.tlsCert);
  const client = new grpc.Client(org.endpoint, grpc.credentials.createSsl(tls), {
    // The peer's certificate names peer1.orgN.example.com; we dial localhost.
    'grpc.ssl_target_name_override': org.hostAlias,
    'grpc.max_receive_message_length': 32 * 1024 * 1024,
  });

  const certPath = await firstFileIn(path.join(org.mspDir, 'signcerts'));
  const keyPath = await firstFileIn(path.join(org.mspDir, 'keystore'));
  const gateway = connect({
    client,
    identity: { mspId, credentials: await fs.readFile(certPath) },
    signer: signers.newPrivateKeySigner(crypto.createPrivateKey(await fs.readFile(keyPath))),
    hash: hash.sha256,
  });

  connection = { gateway, client, network: gateway.getNetwork(CHANNEL), mspId, label: org.label };
  return connection;
}

/**
 * Stream chaincode events from a block.
 *
 * `startBlock` is the whole point. Without it the SDK starts from the next
 * block to be committed, so everything that happened while the projector was
 * down is simply never seen — silently, with no gap to notice afterwards.
 * Passing the checkpoint instead turns a restart into a replay.
 *
 * Payloads are yielded as TEXT, not parsed. Parsing them here would mean
 * JSON.parse, and that rounds any amount above 2^53. events.js parses them in
 * a way that does not.
 */
export async function* streamEvents({ startBlock, signal, mspId = PROJECTOR_MSP }) {
  const { network } = await connectLedger(mspId);
  const options = startBlock === undefined ? {} : { startBlock: BigInt(startBlock) };
  const events = await network.getChaincodeEvents(CHAINCODE, options);
  signal?.addEventListener('abort', () => events.close(), { once: true });

  try {
    for await (const event of events) {
      yield {
        name: event.eventName,
        blockNumber: Number(event.blockNumber),
        transactionId: event.transactionId,
        payloadText: Buffer.from(event.payload).toString('utf8'),
      };
    }
  } catch (err) {
    // Closing the stream makes the in-flight gRPC call fail with CANCELLED.
    // That is this process shutting itself down, not the peer going wrong, and
    // reporting it as an error would mean every clean stop looked like a
    // crash — including in a script checking the exit code.
    if (!(signal?.aborted && isCancelled(err))) throw err;
  } finally {
    events.close();
  }
}

function isCancelled(err) {
  // grpc-js status 1 is CANCELLED.
  return err?.code === 1 || err?.cause?.code === 1 || /CANCELLED/.test(String(err?.message ?? ''));
}

export function closeLedger() {
  if (!connection) return;
  try { connection.gateway.close(); } catch { /* already closed */ }
  try { connection.client.close(); } catch { /* already closed */ }
  connection = null;
}
