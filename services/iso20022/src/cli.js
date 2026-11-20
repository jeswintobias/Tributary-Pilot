#!/usr/bin/env node
// Emit ISO 20022 messages for a transfer read from the ledger.
//
// Takes the chaincode's own GetTransfer output on stdin, so it composes with
// the tools already here rather than growing its own Fabric connection:
//
//   peer chaincode query -C mychannel -n settlement \
//     -c '{"Args":["GetTransfer","TP-1"]}' | node src/cli.js
//
//   node src/cli.js --out ./messages      # write one file per message
//   node src/cli.js --kind pacs.008       # just the instruction
//
// Amounts arrive as JSON numbers, because that is what Go's encoding/json
// does with a uint64. They are pulled back out of the RAW TEXT before
// JSON.parse can turn them into doubles — the same guard the projector uses,
// for the same reason: a uint64 of minor units exceeds what a double holds.

import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { messagesFor } from './messages.js';

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i === -1 ? fallback : process.argv[i + 1];
}

async function readStdin() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf8').trim();
}

/** Parse keeping every integer as its exact decimal text. */
function parseExact(text) {
  return JSON.parse(text, function (key, value, context) {
    if (typeof value === 'number' && Number.isInteger(value) && context?.source !== undefined) {
      return context.source;
    }
    return value;
  });
}

const raw = await readStdin();
if (!raw) {
  console.error('nothing on stdin. Pipe a GetTransfer result in:');
  console.error("  peer chaincode query -C mychannel -n settlement \\");
  console.error("    -c '{\"Args\":[\"GetTransfer\",\"TP-1\"]}' | node src/cli.js");
  process.exit(2);
}

let transfer;
try {
  transfer = parseExact(raw);
} catch (err) {
  console.error(`stdin is not the JSON a GetTransfer returns: ${err.message}`);
  process.exit(2);
}
if (!transfer?.transferID) {
  console.error('that JSON has no transferID; is it a GetTransfer result?');
  process.exit(2);
}

let messages;
try {
  messages = messagesFor(transfer);
} catch (err) {
  console.error(`could not map ${transfer.transferID}: ${err.message}`);
  process.exit(1);
}

const only = arg('kind');
if (only) messages = messages.filter((m) => m.kind === only);
if (!messages.length) {
  console.error(`no ${only} message for a transfer in state ${transfer.state}`);
  process.exit(1);
}

const outDir = arg('out');
if (outDir) {
  mkdirSync(outDir, { recursive: true });
  for (const m of messages) {
    const file = path.join(outDir, `${transfer.transferID}.${m.kind}.xml`);
    writeFileSync(file, `${m.xml}\n`);
    process.stderr.write(`wrote ${file}\n`);
  }
} else {
  for (const m of messages) {
    process.stderr.write(`\n--- ${m.kind}${m.status ? ` (${m.status})` : ''} ---\n`);
    process.stdout.write(`${m.xml}\n`);
  }
}

// An inexact exchange rate is worth saying out loud: the message carries a
// rounded figure where the ledger holds an exact rational.
for (const m of messages) {
  if (m.rateExact === false) {
    process.stderr.write(
      `\nWARNING: ${transfer.transferID}'s rate has no exact decimal, so XchgRate in the\n`
      + 'pacs.008 is rounded. The ledger holds the exact rational; the message cannot.\n');
  }
}
