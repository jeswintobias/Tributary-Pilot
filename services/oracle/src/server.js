// The FX oracle, as a service.
//
// It exists as its own process for one reason: it holds the signing key, and
// the gateway must not. Anyone with that key can sign any rate and the
// chaincode will believe it, so the key lives in exactly one place and
// everything else asks.
//
// Bound to loopback by default, like the gateway, and for the same reason:
// there is no TLS here and a signing service reachable from the network is a
// signing service somebody else can queue up work on.
//
// THE RATES ARE SIMULATED. See src/rates.js.

import http from 'node:http';
import { loadKeys } from './keys.js';
import { rateFor, knownPairs } from './rates.js';
import { signQuote, verifyQuote, applyRate } from './quote.js';

const PORT = Number(process.env.ORACLE_PORT ?? 7710);
const HOST = process.env.ORACLE_HOST ?? '127.0.0.1';
const MAX_BODY = 8 * 1024;
const DEFAULT_TTL = Number(process.env.ORACLE_TTL_SECONDS ?? 300);

const { privateKeyPem, publicKeyPem, oracleID, keyFile } = loadKeys();

const json = (res, status, body) => {
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8' });
  res.end(JSON.stringify(body));
};

async function readBody(req) {
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY) throw new Error('request body is too large');
    chunks.push(chunk);
  }
  if (!chunks.length) return {};
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

const CCY = /^[A-Z]{3}$/;
const DIGITS = /^[0-9]{1,20}$/;

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, `http://${req.headers.host ?? 'localhost'}`);

    if (req.method === 'GET' && url.pathname === '/pubkey') {
      // What an operator feeds to RegisterOracle.
      return json(res, 200, { oracleID, publicKeyPEM: publicKeyPem, pairs: knownPairs() });
    }

    if (req.method === 'POST' && url.pathname === '/quote') {
      const body = await readBody(req);
      const baseCcy = String(body.baseCcy ?? '').toUpperCase();
      const quoteCcy = String(body.quoteCcy ?? '').toUpperCase();
      if (!CCY.test(baseCcy) || !CCY.test(quoteCcy)) {
        return json(res, 400, { error: 'baseCcy and quoteCcy must be three-letter codes' });
      }
      const amount = String(body.amount ?? '0');
      if (!DIGITS.test(amount) || BigInt(amount) <= 0n) {
        return json(res, 400, { error: 'amount must be a whole number of minor units' });
      }
      const ttl = Number.isFinite(Number(body.ttlSeconds)) && Number(body.ttlSeconds) > 0
        ? Math.min(Number(body.ttlSeconds), 900) : DEFAULT_TTL;
      const slippageBps = Number.isFinite(Number(body.maxSlippageBps))
        ? Math.max(0, Math.min(Number(body.maxSlippageBps), 10000)) : 0;

      let rate;
      try {
        rate = rateFor(baseCcy, quoteCcy, { driftBps: Number(body.driftBps ?? 0) });
      } catch (err) {
        return json(res, 404, { error: String(err.message) });
      }

      const quote = {
        quoteID: body.quoteID
          ? String(body.quoteID)
          : `q-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
        oracleID,
        baseCcy,
        quoteCcy,
        rateNum: Number(rate.rateNum),
        rateDen: Number(rate.rateDen),
        expiresAt: Math.floor(Date.now() / 1000) + ttl,
        maxSlippageBps: slippageBps,
      };
      const signature = signQuote(quote, privateKeyPem);
      // Checked here so a broken signature surfaces with a useful message
      // rather than as "not signed by oracle" three services later.
      if (!verifyQuote(quote, signature, publicKeyPem)) {
        return json(res, 500, { error: 'the signature just produced does not verify' });
      }
      return json(res, 200, {
        quote,
        signature,
        expectedPayout: applyRate(amount, rate.rateNum, rate.rateDen),
        simulated: true,
      });
    }

    return json(res, 404, { error: 'no such endpoint' });
  } catch (err) {
    json(res, 400, { error: String(err.message ?? err) });
  }
});

server.listen(PORT, HOST, () => {
  console.log(`FX oracle on http://${HOST}:${PORT}`);
  console.log(`oracle ${oracleID}, key from ${keyFile}`);
  console.log(`pairs: ${knownPairs().join(', ')}`);
  console.log('RATES ARE SIMULATED. This reads no market feed.');
  if (HOST !== '127.0.0.1' && HOST !== '::1') {
    console.warn(`WARNING: bound to ${HOST}. This process holds a signing key and has no TLS.`);
  }
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => server.close(() => process.exit(0)));
}
