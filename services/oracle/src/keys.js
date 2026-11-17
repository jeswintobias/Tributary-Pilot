// The oracle's signing key.
//
// Generated on first use and written with mode 0600. There is no default key
// and no key in the repository: a signing key that ships with the source is a
// signing key everybody has.
//
// The honest limit: this is a file on disk, readable by the account running
// the service. A real rate oracle's key belongs in an HSM, because anyone
// holding it can sign any rate and the chaincode will believe it. That is the
// single point of trust this whole mechanism rests on, and it is stated here
// rather than left to be discovered.

import crypto from 'node:crypto';
import { existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const KEY_FILE = process.env.ORACLE_KEY
  ?? path.join(HERE, '..', 'oracle.key.pem');
export const ORACLE_ID = process.env.ORACLE_ID ?? 'tributary-fx-1';

/** Load the key pair, generating one the first time. */
export function loadKeys() {
  if (!existsSync(KEY_FILE)) {
    const { privateKey } = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
    const pem = privateKey.export({ type: 'pkcs8', format: 'pem' });
    mkdirSync(path.dirname(KEY_FILE), { recursive: true });
    writeFileSync(KEY_FILE, pem, { mode: 0o600 });
  }
  const privateKeyPem = readFileSync(KEY_FILE, 'utf8');
  const publicKeyPem = crypto.createPublicKey(privateKeyPem)
    .export({ type: 'spki', format: 'pem' });
  return { privateKeyPem, publicKeyPem, oracleID: ORACLE_ID, keyFile: KEY_FILE };
}
