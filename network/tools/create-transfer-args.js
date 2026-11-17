#!/usr/bin/env node
// Build the complete `peer chaincode invoke -c` payload for CreateTransfer.
//
// The chaincode now takes a signed quote as a parameter, which is a JSON
// object nested inside the JSON Args array. Assembling that in shell means
// escaping quotes inside quotes inside quotes, and getting it subtly wrong
// produces an error about the signature rather than about the quoting. So it
// is built here, where JSON.stringify does it correctly by construction.
//
//   node create-transfer-args.js --id TP-1 --sender 220000 --sender-ccy AED \
//     --payout 5003482 --payout-ccy INR --expires 1794000000 \
//     --payout-org Org2MSP --payee sha256:ref [--slippage 25] [--drift 0]
//
// Mints a fresh quote from the oracle's key, because a quote is single use on
// chain. Prints the payload on stdout; everything else goes to stderr.

import crypto from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ORACLE = path.join(HERE, '..', '..', 'services', 'oracle', 'src');

const { loadKeys } = await import(path.join(ORACLE, 'keys.js'));
const { rateFor } = await import(path.join(ORACLE, 'rates.js'));
const { signQuote, applyRate } = await import(path.join(ORACLE, 'quote.js'));

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i === -1 ? fallback : process.argv[i + 1];
}

const transferID = arg('id');
const senderAmount = String(arg('sender'));
const senderCcy = String(arg('sender-ccy', 'AED')).toUpperCase();
const payoutCcy = String(arg('payout-ccy', 'INR')).toUpperCase();
const expiresAt = String(arg('expires'));
const payoutOrg = String(arg('payout-org', 'Org2MSP'));
const payee = String(arg('payee'));
const slippageBps = Number(arg('slippage', 0));
const driftBps = Number(arg('drift', 0));
const ttl = Number(arg('ttl', 600));

if (!transferID || !senderAmount || !expiresAt || !payee) {
  console.error('usage: --id --sender --expires --payee [--sender-ccy --payout-ccy '
    + '--payout --payout-org --slippage --drift --ttl]');
  process.exit(2);
}

// --forge signs with a throwaway key instead of the oracle's. The quote is
// otherwise perfect: right rate, right currencies, not expired. It exists so a
// demo can show the chaincode refusing a well-formed forgery rather than
// refusing something obviously malformed.
const forge = process.argv.includes('--forge');
const real = loadKeys();
const privateKeyPem = forge
  ? crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' })
    .privateKey.export({ type: 'pkcs8', format: 'pem' })
  : real.privateKeyPem;
const oracleID = real.oracleID;
const { rateNum, rateDen } = rateFor(senderCcy, payoutCcy, { driftBps });

// The payout leg defaults to exactly what the rate implies. A caller can pass
// --payout to force a different one, which is how the demo shows the contract
// refusing legs that do not match the quote.
const impliedPayout = applyRate(senderAmount, rateNum, rateDen);
const payoutAmount = String(arg('payout', impliedPayout));

const quote = {
  quoteID: String(arg('quote-id', `q-${transferID}-${Date.now()}`)).slice(0, 64),
  oracleID,
  baseCcy: senderCcy,
  quoteCcy: payoutCcy,
  rateNum: Number(rateNum),
  rateDen: Number(rateDen),
  expiresAt: Number(arg('quote-expires', Math.floor(Date.now() / 1000) + ttl)),
  maxSlippageBps: slippageBps,
};
const signature = signQuote(quote, privateKeyPem);

process.stderr.write(
  `quote ${quote.quoteID} at ${rateNum}/${rateDen} (${slippageBps}bps): `
  + `${senderAmount} ${senderCcy} -> implied ${impliedPayout}, using ${payoutAmount} ${payoutCcy}`
  + `${forge ? ' [SIGNED BY A KEY THE CHAIN DOES NOT KNOW]' : ''}\n`);

process.stdout.write(JSON.stringify({
  Args: [
    'CreateTransfer', transferID, senderAmount, senderCcy,
    payoutAmount, payoutCcy, JSON.stringify(quote), signature,
    expiresAt, payoutOrg, payee,
  ],
}));
