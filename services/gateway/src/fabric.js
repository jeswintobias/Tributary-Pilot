// Connection to the live DRUNIX network through the Fabric Gateway SDK.
//
// Three identities are held at once — SenderOrg, PayoutBankOrg and PlatformOrg
// — so the UI can submit as any of them. That is the point: PlatformOrg's
// read-only status is something you can try and watch fail, rather than a claim
// in a README.
//
// Nothing here is mocked. Every call goes to a peer.

import { promises as fs } from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import crypto from 'node:crypto';
import grpc from '@grpc/grpc-js';
import { connect, hash, signers } from '@hyperledger/fabric-gateway';

const TRIBUTARY_HOME = process.env.TRIBUTARY_HOME ?? path.join(os.homedir(), '.tributary-pilot');
const TEST_NETWORK = process.env.TEST_NETWORK_DIR
  ?? path.join(TRIBUTARY_HOME, 'drunix', 'drunix-network', 'test-network');
const PLATFORM_HOME = path.join(TRIBUTARY_HOME, 'platformorg');

export const CHANNEL = process.env.CHANNEL_NAME ?? 'mychannel';
export const CHAINCODE = process.env.CC_NAME ?? 'settlement';

const peerOrg = (n) => path.join(TEST_NETWORK, 'organizations', 'peerOrganizations', `org${n}.example.com`);

// PlatformOrg runs no peer of its own — a regulator observer does not endorse —
// so it connects through SenderOrg's Lite Peer with its own signing material.
// Peer 0 is the Lite Peer, the one that endorses and holds the chaincode; peer
// 1 is the Committing Peer and has none.
export const ORGS = {
  Org1MSP: {
    label: 'SenderOrg',
    role: 'UAE money transfer operator',
    mspId: 'Org1MSP',
    endpoint: 'localhost:7051',
    hostAlias: 'peer0.org1.example.com',
    tlsCert: path.join(peerOrg(1), 'tlsca', 'tlsca.org1.example.com-cert.pem'),
    mspDir: path.join(peerOrg(1), 'users', 'Admin@org1.example.com', 'msp'),
    eventEndpoint: 'localhost:7061',
    eventHostAlias: 'peer1.org1.example.com',
  },
  Org2MSP: {
    label: 'PayoutBankOrg',
    role: 'Indian payout bank',
    mspId: 'Org2MSP',
    endpoint: 'localhost:9051',
    hostAlias: 'peer0.org2.example.com',
    tlsCert: path.join(peerOrg(2), 'tlsca', 'tlsca.org2.example.com-cert.pem'),
    mspDir: path.join(peerOrg(2), 'users', 'Admin@org2.example.com', 'msp'),
    eventEndpoint: 'localhost:9061',
    eventHostAlias: 'peer1.org2.example.com',
  },
  Org3MSP: {
    label: 'PlatformOrg',
    role: 'NPCI platform node / regulator observer',
    mspId: 'Org3MSP',
    endpoint: 'localhost:7051',
    hostAlias: 'peer0.org1.example.com',
    tlsCert: path.join(peerOrg(1), 'tlsca', 'tlsca.org1.example.com-cert.pem'),
    mspDir: path.join(PLATFORM_HOME, 'organizations', 'peerOrganizations',
      'org3.example.com', 'users', 'Admin@org3.example.com', 'msp'),
    eventEndpoint: 'localhost:7061',
    eventHostAlias: 'peer1.org1.example.com',
    readOnly: true,
  },
};

// Commit needs both parties' endorsement — AND(Org1MSP.peer, Org2MSP.peer).
// Naming them explicitly avoids depending on service discovery, which DRUNIX's
// split Lite/Committing peer topology does not advertise the way stock Fabric
// does.
const ENDORSERS = ['Org1MSP', 'Org2MSP'];

async function firstFileIn(dir) {
  const entries = await fs.readdir(dir);
  const file = entries.find((e) => !e.startsWith('.'));
  if (!file) throw new Error(`no files in ${dir}`);
  return path.join(dir, file);
}

async function identityFor(org) {
  const certPath = await firstFileIn(path.join(org.mspDir, 'signcerts'));
  const keyPath = await firstFileIn(path.join(org.mspDir, 'keystore'));
  const credentials = await fs.readFile(certPath);
  const privateKeyPem = await fs.readFile(keyPath);
  return {
    identity: { mspId: org.mspId, credentials },
    signer: signers.newPrivateKeySigner(crypto.createPrivateKey(privateKeyPem)),
  };
}

async function clientFor(org, { events = false } = {}) {
  const tls = await fs.readFile(org.tlsCert);
  const endpoint = events ? org.eventEndpoint : org.endpoint;
  const alias = events ? org.eventHostAlias : org.hostAlias;
  return new grpc.Client(endpoint, grpc.credentials.createSsl(tls), {
    // The peer's certificate names peerN.orgN.example.com; we dial localhost.
    'grpc.ssl_target_name_override': alias,
    'grpc.max_receive_message_length': 32 * 1024 * 1024,
  });
}

const connections = new Map();

/** Connect (once) as the given MSP and return its Gateway contract. */
export async function contractFor(mspId) {
  if (connections.has(mspId)) return connections.get(mspId);

  const org = ORGS[mspId];
  if (!org) throw new Error(`unknown organisation ${mspId}`);

  const client = await clientFor(org);
  const { identity, signer } = await identityFor(org);

  const gateway = connect({
    client,
    identity,
    signer,
    hash: hash.sha256,
    // Emulated containers are slow; these are generous on purpose.
    evaluateOptions: () => ({ deadline: Date.now() + 15_000 }),
    endorseOptions: () => ({ deadline: Date.now() + 30_000 }),
    submitOptions: () => ({ deadline: Date.now() + 30_000 }),
    commitStatusOptions: () => ({ deadline: Date.now() + 90_000 }),
  });

  const contract = gateway.getNetwork(CHANNEL).getContract(CHAINCODE);
  const handle = { contract, gateway, client, network: gateway.getNetwork(CHANNEL) };
  connections.set(mspId, handle);
  return handle;
}

/** Read-only call. Does not reach the orderer and changes nothing. */
export async function evaluate(mspId, fn, args = []) {
  const { contract } = await contractFor(mspId);
  const bytes = await contract.evaluateTransaction(fn, ...args);
  const text = Buffer.from(bytes).toString('utf8');
  return text.length ? JSON.parse(text) : null;
}

/**
 * State-changing call: endorse, order, and wait for the block to commit.
 *
 * A transaction the chaincode rejects throws here. One the Validation Service
 * rejects — an under-endorsed commit, say — is reported by the commit status,
 * not by endorsement, so both are surfaced.
 */
export async function submit(mspId, fn, args = []) {
  const { contract } = await contractFor(mspId);
  const proposal = contract.newProposal(fn, { arguments: args, endorsingOrganizations: ENDORSERS });
  const transaction = await proposal.endorse();
  const submitted = await transaction.submit();
  const status = await submitted.getStatus();
  if (!status.successful) {
    throw new Error(`transaction ${status.transactionId} was invalidated: ${status.code}`);
  }
  const text = Buffer.from(submitted.getResult()).toString('utf8');
  return { transactionId: status.transactionId, result: text.length ? JSON.parse(text) : null };
}

// Event delivery needs a peer that HAS a ledger.
//
// DRUNIX splits the peer in two, and only one half can serve blocks. The Lite
// Peer endorses and holds the chaincode but is stateless, so a chaincode-event
// subscription against it is accepted and then silently delivers nothing — not
// even from block 0. The Committing Peer holds the ledger and delivers
// normally. So transactions are endorsed on one peer and events are read from
// another, which has no equivalent in stock Fabric.
const eventConnections = new Map();

async function eventNetworkFor(mspId) {
  if (eventConnections.has(mspId)) return eventConnections.get(mspId);
  const org = ORGS[mspId];
  if (!org?.eventEndpoint) throw new Error(`no event peer configured for ${mspId}`);

  const client = await clientFor(org, { events: true });
  const { identity, signer } = await identityFor(org);
  const gateway = connect({
    client, identity, signer, hash: hash.sha256,
    evaluateOptions: () => ({ deadline: Date.now() + 15_000 }),
  });
  const handle = { network: gateway.getNetwork(CHANNEL), gateway, client };
  eventConnections.set(mspId, handle);
  return handle;
}

/** Chaincode events, from the current block onward. */
export async function* chaincodeEvents(mspId = 'Org1MSP', signal) {
  const { network } = await eventNetworkFor(mspId);
  const events = await network.getChaincodeEvents(CHAINCODE);
  signal?.addEventListener('abort', () => events.close(), { once: true });
  for await (const event of events) {
    yield {
      name: event.eventName,
      blockNumber: Number(event.blockNumber),
      transactionId: event.transactionId,
      payload: JSON.parse(Buffer.from(event.payload).toString('utf8')),
    };
  }
}

export function closeAll() {
  for (const map of [connections, eventConnections]) {
    for (const { gateway, client } of map.values()) {
      try { gateway.close(); } catch { /* already closed */ }
      try { client.close(); } catch { /* already closed */ }
    }
    map.clear();
  }
}
