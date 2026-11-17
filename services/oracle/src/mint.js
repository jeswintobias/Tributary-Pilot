#!/usr/bin/env node
// Mint one signed quote and print it as JSON.
//
// This is what the demo scripts call: the chaincode will not create a transfer
// without a signed rate, so a shell script needs a way to get one.
//
//   node src/mint.js --base AED --quote INR --amount 220000 --ttl 300
//
// Prints { quote, signature, expectedPayout } on stdout. Everything else goes
// to stderr so the output can be piped straight into jq or peer chaincode.

import { loadKeys } from './keys.js';
import { rateFor } from './rates.js';
import { signQuote, applyRate, verifyQuote } from './quote.js';

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i === -1 ? fallback : process.argv[i + 1];
}

const base = String(arg('base', 'AED')).toUpperCase();
const quoteCcy = String(arg('quote', 'INR')).toUpperCase();
const amount = String(arg('amount', '220000'));
const ttl = Number(arg('ttl', 300));
const slippageBps = Number(arg('slippage', 0));
const driftBps = Number(arg('drift', 0));
const quoteID = arg('id', `q-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`);

const { privateKeyPem, publicKeyPem, oracleID } = loadKeys();
const { rateNum, rateDen } = rateFor(base, quoteCcy, { driftBps });

const quote = {
  quoteID,
  oracleID,
  baseCcy: base,
  quoteCcy,
  rateNum: Number(rateNum),
  rateDen: Number(rateDen),
  expiresAt: Math.floor(Date.now() / 1000) + ttl,
  maxSlippageBps: slippageBps,
};

const signature = signQuote(quote, privateKeyPem);
// Verified here before it is handed out. A quote that fails locally would fail
// on chain with a far less useful message.
if (!verifyQuote(quote, signature, publicKeyPem)) {
  console.error('the signature this service just produced does not verify; refusing to emit it');
  process.exit(1);
}

const expectedPayout = applyRate(amount, rateNum, rateDen);
process.stderr.write(
  `minted ${quoteID}: ${amount} ${base} -> ${expectedPayout} ${quoteCcy} `
  + `at ${rateNum}/${rateDen}, ${slippageBps}bps slippage, ${ttl}s ttl (SIMULATED rate)\n`);

process.stdout.write(`${JSON.stringify({ quote, signature, expectedPayout })}\n`);
