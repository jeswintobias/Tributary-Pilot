#!/usr/bin/env node
// Print the oracle's public key and ID, for RegisterOracle.
import { loadKeys } from './keys.js';

const { publicKeyPem, oracleID, keyFile } = loadKeys();
if (process.argv.includes('--id')) {
  process.stdout.write(`${oracleID}\n`);
} else {
  process.stderr.write(`oracle ${oracleID}, key from ${keyFile}\n`);
  process.stdout.write(publicKeyPem);
}
