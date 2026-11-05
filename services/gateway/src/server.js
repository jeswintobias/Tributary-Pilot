// HTTP API and static host for the corridor view.
//
// Every endpoint goes to the live ledger. There is no in-memory store, no seed
// data and no mock: if the network is down, this service returns errors rather
// than something that looks like a working corridor.

import http from 'node:http';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { evaluate, submit, chaincodeEvents, closeAll, ORGS, CHANNEL, CHAINCODE } from './fabric.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC = path.join(HERE, '..', 'public');
const PORT = Number(process.env.PORT ?? 7700);

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.svg': 'image/svg+xml',
};

const json = (res, status, body) => {
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8' });
  res.end(JSON.stringify(body));
};

async function readBody(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  if (!chunks.length) return {};
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

// The chaincode's own message is the interesting part of a failure — "caller
// Org3MSP is not a party to transfer" is the demo. Unwrap it from the SDK's
// envelope rather than flattening everything to "500".
function describeError(err) {
  const detail = err?.details?.[0]?.message ?? err?.message ?? String(err);
  const chaincodeMessage = /message:"([^"]*)"/.exec(detail)?.[1]
    ?? /chaincode response 500, (.*)$/.exec(detail)?.[1]
    ?? detail;
  return chaincodeMessage.replace(/\\"/g, '"').trim();
}

const asOrg = (value) => {
  const mspId = value ?? 'Org1MSP';
  if (!ORGS[mspId]) throw new Error(`unknown organisation ${mspId}`);
  return mspId;
};

const ACTIONS = new Set(['Accept', 'Prepare', 'Commit', 'Abort']);

async function handleApi(req, res, url) {
  // Who is on the channel, and which of them may write.
  if (req.method === 'GET' && url.pathname === '/api/orgs') {
    return json(res, 200, {
      channel: CHANNEL,
      chaincode: CHAINCODE,
      orgs: Object.entries(ORGS).map(([mspId, o]) => ({
        mspId, label: o.label, role: o.role, readOnly: Boolean(o.readOnly),
      })),
    });
  }

  // The whole corridor, read from the ledger.
  if (req.method === 'GET' && url.pathname === '/api/state') {
    const as = asOrg(url.searchParams.get('as'));
    const [transfers, balances] = await Promise.all([
      evaluate(as, 'ListTransfers'),
      evaluate(as, 'ListBalances'),
    ]);
    return json(res, 200, { as, transfers: transfers ?? [], balances: balances ?? [] });
  }

  if (req.method === 'POST' && url.pathname === '/api/seed') {
    const { as, ccy, amount } = await readBody(req);
    const out = await submit(asOrg(as), 'SeedBalance', [String(ccy), String(amount)]);
    return json(res, 200, out);
  }

  if (req.method === 'POST' && url.pathname === '/api/transfers') {
    const b = await readBody(req);
    const out = await submit(asOrg(b.as), 'CreateTransfer', [
      String(b.transferID), String(b.senderLegAmount), String(b.senderCcy),
      String(b.payoutLegAmount), String(b.payoutCcy), String(b.quoteID),
      String(b.expiresAt), String(b.payoutOrg), String(b.payoutPayeeRef),
    ]);
    return json(res, 200, out);
  }

  // /api/transfers/<id>/<Accept|Prepare|Commit|Abort>
  const action = /^\/api\/transfers\/([^/]+)\/([A-Za-z]+)$/.exec(url.pathname);
  if (req.method === 'POST' && action) {
    const [, id, name] = action;
    const fn = name[0].toUpperCase() + name.slice(1).toLowerCase();
    if (!ACTIONS.has(fn)) return json(res, 404, { error: `unknown action ${name}` });
    const { as } = await readBody(req);
    const out = await submit(asOrg(as), fn, [decodeURIComponent(id)]);
    return json(res, 200, out);
  }

  if (req.method === 'GET' && url.pathname === '/api/transfer') {
    const as = asOrg(url.searchParams.get('as'));
    const id = url.searchParams.get('id');
    return json(res, 200, await evaluate(as, 'GetTransfer', [id]));
  }

  // Chaincode events, streamed as they commit.
  if (req.method === 'GET' && url.pathname === '/api/events') {
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    });
    res.write(': connected\n\n');
    const controller = new AbortController();
    req.on('close', () => controller.abort());
    try {
      for await (const event of chaincodeEvents('Org1MSP', controller.signal)) {
        res.write(`data: ${JSON.stringify(event)}\n\n`);
      }
    } catch {
      // The stream ends when the client disconnects or the peer closes it.
    }
    return res.end();
  }

  return json(res, 404, { error: 'no such endpoint' });
}

async function serveStatic(res, pathname) {
  const rel = pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, '');
  const file = path.join(PUBLIC, rel);
  // Never serve outside public/.
  if (!file.startsWith(PUBLIC)) return json(res, 403, { error: 'forbidden' });
  try {
    const body = await fs.readFile(file);
    res.writeHead(200, { 'content-type': TYPES[path.extname(file)] ?? 'application/octet-stream' });
    res.end(body);
  } catch {
    json(res, 404, { error: 'not found' });
  }
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);
  try {
    if (url.pathname.startsWith('/api/')) return await handleApi(req, res, url);
    return await serveStatic(res, url.pathname);
  } catch (err) {
    const message = describeError(err);
    console.error(`${req.method} ${url.pathname} -> ${message}`);
    // 422: the ledger understood the call and refused it. That is a result, not
    // a server fault, and the UI shows it as such.
    json(res, 422, { error: message });
  }
});

server.listen(PORT, () => {
  console.log(`Tributary corridor view on http://localhost:${PORT}`);
  console.log(`channel ${CHANNEL} · chaincode ${CHAINCODE} · live ledger, nothing mocked`);
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => { closeAll(); server.close(() => process.exit(0)); });
}
