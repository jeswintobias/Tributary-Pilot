// HTTP API and static host for the corridor view.
//
// Every endpoint goes to the live ledger. There is no in-memory store, no seed
// data and no mock: if the network is down, this service returns errors rather
// than something that looks like a working corridor.
//
// Security posture, stated plainly because the previous version had none:
//   - binds to loopback only unless GATEWAY_HOST says otherwise
//   - every call needs a bearer token, and the token decides which
//     organisation the call acts as — the request body no longer chooses
//   - requests are rate limited per token and bounded in size
//   - SeedBalance, which mints money from nothing, only exists in dev mode
//
// What it still does NOT do, and should before it faces a network: TLS, and
// one gateway per organisation instead of one holding all three keys. Both are
// recorded in docs/THREAT_MODEL.md under "The service layer".

import http from 'node:http';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  evaluate, submit, chaincodeEvents, chainHeight, closeAll, ORGS, CHANNEL, CHAINCODE,
} from './fabric.js';
import {
  loadTokens, mspForToken, tokenFromRequest, tokenMap, devTokensAllowed, rateLimit, DEV_MODE,
} from './auth.js';
import { pipelineState, projectionAvailable, closeProjection, PROJECTION_DB } from './readmodel.js';
import {
  validateCreateTransfer, validateSeed, validateTransferId, validateObligation,
  validateWindowSeq, validateRateQuery, BadRequest, MAX_BODY_BYTES,
} from './validate.js';
import { requestQuote, oracleIdentity, OracleUnavailable, ORACLE_ENDPOINT } from './oracle.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC = path.join(HERE, '..', 'public');
const PORT = Number(process.env.PORT ?? 7700);
// Loopback by default. Binding anywhere else is a deliberate act, and the
// startup banner says so out loud.
const HOST = process.env.GATEWAY_HOST ?? '127.0.0.1';

// Must match NettingWindowSeconds in chaincode/settlement/settlement/pool.go.
// Duplicated constants drift, so this one is checked against the ledger at
// startup rather than trusted — see verifyWindowLength below.
const NETTING_WINDOW_SECONDS = 60;

// How many past windows the view scans for one still awaiting settlement.
// Bounded because each is a query: far enough back to catch a stalled run,
// not so far that the page costs a hundred round trips.
const NETTING_CLOSE_LOOKBACK = 30;

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
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY_BYTES) throw new BadRequest('request body is too large');
    chunks.push(chunk);
  }
  if (!chunks.length) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    throw new BadRequest('request body is not valid JSON');
  }
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

const ACTIONS = new Set(['Accept', 'Prepare', 'Commit', 'Abort']);

async function handleApi(req, res, url) {
  // --- public: the roster, so the sign-in screen can be drawn -------------
  if (req.method === 'GET' && url.pathname === '/api/orgs') {
    return json(res, 200, {
      channel: CHANNEL,
      chaincode: CHAINCODE,
      devMode: DEV_MODE,
      orgs: Object.entries(ORGS).map(([mspId, o]) => ({
        mspId, label: o.label, role: o.role, readOnly: Boolean(o.readOnly),
      })),
    });
  }

  // --- dev only, loopback only: hand the page its tokens ------------------
  if (req.method === 'GET' && url.pathname === '/api/dev/tokens') {
    if (!devTokensAllowed(req)) return json(res, 404, { error: 'no such endpoint' });
    return json(res, 200, { tokens: tokenMap() });
  }

  // --- everything below needs a token ------------------------------------
  const token = tokenFromRequest(req, url);
  const as = mspForToken(token);
  if (!as) {
    res.writeHead(401, {
      'content-type': 'application/json; charset=utf-8',
      'www-authenticate': 'Bearer realm="tributary"',
    });
    return res.end(JSON.stringify({ error: 'a valid bearer token is required' }));
  }

  const limit = rateLimit(token);
  if (!limit.ok) {
    res.writeHead(429, {
      'content-type': 'application/json; charset=utf-8',
      'retry-after': String(limit.retryAfter),
    });
    return res.end(JSON.stringify({ error: `rate limit exceeded, retry in ${limit.retryAfter}s` }));
  }

  // Who you are is decided here, by the token. Nothing in the body can change
  // it, which is the whole point of this rewrite.
  if (req.method === 'GET' && url.pathname === '/api/session') {
    const org = ORGS[as];
    return json(res, 200, { mspId: as, label: org.label, role: org.role, readOnly: Boolean(org.readOnly) });
  }

  if (req.method === 'GET' && url.pathname === '/api/state') {
    const [transfers, balances] = await Promise.all([
      evaluate(as, 'ListTransfers'),
      evaluate(as, 'ListBalances'),
    ]);
    return json(res, 200, { as, transfers: transfers ?? [], balances: balances ?? [] });
  }

  // --- netting ------------------------------------------------------------
  // The corridor view needs three things to show netting honestly: which
  // window is accruing now, what it would settle to if it closed, and what the
  // windows that already closed actually did. All three come off the ledger.
  if (req.method === 'GET' && url.pathname === '/api/netting') {
    const ccy = (url.searchParams.get('ccy') ?? 'AED').toUpperCase();
    const seq = Number(await evaluate(as, 'CurrentWindow'));
    const [current, settled] = await Promise.all([
      evaluate(as, 'GetWindow', [ccy, String(seq)]),
      evaluate(as, 'ListWindows'),
    ]);

    // Find the OLDEST past window still carrying obligations, not merely the
    // one before this. Offering only the previous window strands anything
    // older: nobody has to close a window, so a run that stops halfway leaves
    // obligations — and the reservations backing them — with no route to
    // settlement through this page. The operator should always be pointed at
    // the oldest outstanding one.
    const lookback = Math.min(seq, NETTING_CLOSE_LOOKBACK);
    const past = await Promise.all(
      Array.from({ length: lookback }, (_, i) => seq - lookback + i)
        .map((s) => evaluate(as, 'GetWindow', [ccy, String(s)]).catch(() => null)),
    );
    const closable = past.find((w) => w && w.state === 'OPEN' && Number(w.obligations) > 0)
      ?? past.find((w) => w && w.state === 'OPEN')
      ?? null;
    const pending = past.filter((w) => w && w.state === 'OPEN' && Number(w.obligations) > 0).length;
    const exposures = await Promise.all(
      Object.keys(ORGS).map(async (mspId) => ({
        mspId,
        ...(await evaluate(as, 'GetExposure', [mspId, ccy])),
      })),
    );
    return json(res, 200, {
      ccy,
      currentSeq: seq,
      windowSeconds: NETTING_WINDOW_SECONDS,
      current,
      closable,
      pending,
      lookback,
      settled: (settled ?? []).filter((w) => w.ccy === ccy),
      exposures,
    });
  }

  if (req.method === 'GET' && url.pathname === '/api/obligations') {
    const ccy = (url.searchParams.get('ccy') ?? 'AED').toUpperCase();
    const seq = validateWindowSeq(url.searchParams.get('seq'));
    return json(res, 200, {
      ccy, seq,
      obligations: (await evaluate(as, 'ListObligations', [ccy, String(seq)])) ?? [],
    });
  }

  if (req.method === 'POST' && url.pathname === '/api/obligations') {
    const b = validateObligation(await readBody(req));
    // The debtor is the signing organisation, decided by the token. There is no
    // field for it here and there must not be.
    return json(res, 200, await submit(as, 'SubmitObligation', [
      b.obligationID, b.creditor, b.ccy, b.amount,
    ]));
  }

  // /api/windows/<CCY>/<seq>/close
  const closeWindow = /^\/api\/windows\/([A-Za-z]{3})\/([0-9]{1,20})\/close$/.exec(url.pathname);
  if (req.method === 'POST' && closeWindow) {
    const [, rawCcy, rawSeq] = closeWindow;
    const seq = validateWindowSeq(rawSeq);
    await readBody(req);
    // Any member may close a window: the netting is a pure function of state
    // that is already frozen, so there is no privileged closer to authorise.
    return json(res, 200, await submit(as, 'CloseWindow', [rawCcy.toUpperCase(), String(seq)]));
  }

  // --- the query side -----------------------------------------------------
  // Read from the projector's store rather than the peers. The response says
  // how far behind it is, so nothing here can be mistaken for live.
  if (req.method === 'GET' && url.pathname === '/api/pipeline') {
    let ledgerBlock = null;
    try {
      // Height is the number of blocks, so the newest block's number is one
      // less. Best effort: if the peers are unreachable the projection is
      // still worth showing, with the lag marked unknown rather than invented.
      ledgerBlock = (await chainHeight(as)) - 1;
    } catch { /* lag stays null, which the UI renders as unknown */ }
    return json(res, 200, pipelineState({ ledgerBlock }));
  }

  if (req.method === 'GET' && url.pathname === '/api/transfer') {
    const id = validateTransferId(url.searchParams.get('id'));
    return json(res, 200, await evaluate(as, 'GetTransfer', [id]));
  }

  // SeedBalance mints money from nothing. It is Phase 0 scaffolding that the
  // token contract replaces, and it has no business being reachable in a build
  // anyone else can touch.
  if (req.method === 'POST' && url.pathname === '/api/seed') {
    if (!DEV_MODE) {
      return json(res, 403, {
        error: 'SeedBalance is disabled; start with GATEWAY_DEV=1 to enable it in development',
      });
    }
    const { ccy, amount } = validateSeed(await readBody(req));
    return json(res, 200, await submit(as, 'SeedBalance', [ccy, amount]));
  }

  // --- FX ------------------------------------------------------------------
  // A rate enquiry. Signs nothing, moves nothing; it exists so the page can
  // show what a transfer would convert to before anyone commits to it.
  if (req.method === 'GET' && url.pathname === '/api/rate') {
    const q = validateRateQuery(url.searchParams);
    const quoted = await requestQuote({
      baseCcy: q.baseCcy, quoteCcy: q.quoteCcy,
      amount: q.amount, maxSlippageBps: q.maxSlippageBps,
    });
    return json(res, 200, {
      baseCcy: q.baseCcy, quoteCcy: q.quoteCcy, amount: q.amount,
      rateNum: quoted.quote.rateNum, rateDen: quoted.quote.rateDen,
      expectedPayout: quoted.expectedPayout,
      expiresAt: quoted.quote.expiresAt,
      maxSlippageBps: quoted.quote.maxSlippageBps,
      oracleID: quoted.quote.oracleID,
      simulated: Boolean(quoted.simulated),
    });
  }

  // The oracle's identity and key, so an operator can register it on chain.
  if (req.method === 'GET' && url.pathname === '/api/oracle') {
    return json(res, 200, { endpoint: ORACLE_ENDPOINT, ...(await oracleIdentity()) });
  }

  if (req.method === 'POST' && url.pathname === '/api/transfers') {
    const b = validateCreateTransfer(await readBody(req));
    // A fresh quote per transfer. The chaincode treats a quote as single use
    // — a dealt rate, not a standing one — so reusing one here would be
    // refused on the second transfer anyway.
    const quoted = await requestQuote({
      baseCcy: b.senderCcy, quoteCcy: b.payoutCcy,
      amount: b.senderLegAmount, maxSlippageBps: b.maxSlippageBps,
    });
    return json(res, 200, {
      ...(await submit(as, 'CreateTransfer', [
        b.transferID, b.senderLegAmount, b.senderCcy,
        b.payoutLegAmount, b.payoutCcy,
        JSON.stringify(quoted.quote), quoted.signature,
        b.expiresAt, b.payoutOrg, b.payoutPayeeRef,
      ])),
      quote: quoted.quote,
      expectedPayout: quoted.expectedPayout,
    });
  }

  // Whether a beneficiary may be paid, and why not. A query, so a user is told
  // before they try rather than after.
  if (req.method === 'GET' && url.pathname === '/api/beneficiary') {
    const ref = url.searchParams.get('ref') ?? '';
    if (!ref) throw new BadRequest('ref is required');
    return json(res, 200, await evaluate(as, 'CheckBeneficiary', [ref]));
  }

  if (req.method === 'GET' && url.pathname === '/api/attestations') {
    return json(res, 200, { attestations: (await evaluate(as, 'ListAttestations')) ?? [] });
  }

  // /api/transfers/<id>/<Accept|Prepare|Commit|Abort>
  const action = /^\/api\/transfers\/([^/]+)\/([A-Za-z]+)$/.exec(url.pathname);
  if (req.method === 'POST' && action) {
    const [, rawId, name] = action;
    const id = validateTransferId(decodeURIComponent(rawId));
    const fn = name[0].toUpperCase() + name.slice(1).toLowerCase();
    if (!ACTIONS.has(fn)) return json(res, 404, { error: `unknown action ${name}` });
    await readBody(req); // drained and ignored: identity comes from the token
    return json(res, 200, await submit(as, fn, [id]));
  }

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
      for await (const event of chaincodeEvents(as, controller.signal)) {
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
  if (!file.startsWith(PUBLIC)) return json(res, 403, { error: 'forbidden' });
  try {
    const body = await fs.readFile(file);
    res.writeHead(200, {
      'content-type': TYPES[path.extname(file)] ?? 'application/octet-stream',
      'x-content-type-options': 'nosniff',
    });
    res.end(body);
  } catch {
    json(res, 404, { error: 'not found' });
  }
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host ?? 'localhost'}`);
  try {
    if (url.pathname.startsWith('/api/')) return await handleApi(req, res, url);
    return await serveStatic(res, url.pathname);
  } catch (err) {
    if (err instanceof BadRequest) return json(res, 400, { error: err.message });
    // 503: the ledger is fine, a service this one depends on is not. A
    // transfer refused because no rate could be signed is not a bad request
    // and not a ledger rejection.
    if (err instanceof OracleUnavailable) return json(res, 503, { error: err.message });
    const message = describeError(err);
    console.error(`${req.method} ${url.pathname} -> ${message}`);
    // 422: the ledger understood the call and refused it. That is a result, not
    // a server fault, and the UI shows it as such.
    json(res, 422, { error: message });
  }
});

// The chaincode derives its window number as txTime / NettingWindowSeconds and
// exposes the result, so the window length can be recovered from the chain
// instead of assumed. If this service's copy of the constant is wrong, every
// window number it shows would be wrong too — and silently.
async function verifyWindowLength() {
  const anyOrg = Object.keys(ORGS)[0];
  const seq = Number(await evaluate(anyOrg, 'CurrentWindow'));
  if (!Number.isFinite(seq) || seq <= 0) return null;
  // seq = floor(now / W), and at epoch scale that pins W to a single integer.
  const derived = Math.round(Math.floor(Date.now() / 1000) / seq);
  return { seq, derived };
}

const { source, generated } = await loadTokens(Object.keys(ORGS));

server.listen(PORT, HOST, () => {
  console.log(`Tributary corridor view on http://${HOST}:${PORT}`);
  console.log(`channel ${CHANNEL} · chaincode ${CHAINCODE} · live ledger, nothing mocked`);
  console.log(`auth: bearer tokens from ${source}${generated ? ' (generated just now)' : ''}`);
  if (DEV_MODE) {
    console.log('GATEWAY_DEV=1 — SeedBalance is enabled and the page may fetch its own tokens from loopback');
  } else {
    console.log('production posture — SeedBalance disabled; sign in with a token from the file above');
  }
  if (HOST !== '127.0.0.1' && HOST !== '::1') {
    console.warn(`WARNING: bound to ${HOST}, not loopback. There is no TLS here — put a proxy in front.`);
  }
  if (projectionAvailable()) {
    console.log(`read model: ${PROJECTION_DB} (read-only)`);
  } else {
    console.log('read model: not present — start services/projector to build it');
  }
  verifyWindowLength().then((check) => {
    if (!check) return;
    if (check.derived === NETTING_WINDOW_SECONDS) {
      console.log(`netting: ${NETTING_WINDOW_SECONDS}s windows confirmed against the ledger (window ${check.seq})`);
    } else {
      console.warn(
        `WARNING: this service assumes ${NETTING_WINDOW_SECONDS}s netting windows, but the `
        + `ledger's window ${check.seq} implies ${check.derived}s. Window numbers shown here will be wrong.`);
    }
  }).catch((err) => {
    console.warn(`netting: could not confirm the window length against the ledger (${describeError(err)})`);
  });
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => { closeAll(); closeProjection(); server.close(() => process.exit(0)); });
}
