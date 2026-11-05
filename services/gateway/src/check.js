// Connectivity probe: proves the Fabric Gateway SDK can reach DRUNIX before
// anything is built on top of it. Run with `npm run check`.
import { evaluate, closeAll, ORGS, CHANNEL, CHAINCODE } from './fabric.js';

const ok = (m) => console.log(`\x1b[32mOK\x1b[0m   ${m}`);
const bad = (m) => console.log(`\x1b[31mFAIL\x1b[0m ${m}`);

let failures = 0;
console.log(`channel=${CHANNEL} chaincode=${CHAINCODE}\n`);

for (const [mspId, org] of Object.entries(ORGS)) {
  try {
    const transfers = await evaluate(mspId, 'ListTransfers');
    const balances = await evaluate(mspId, 'ListBalances');
    ok(`${mspId} (${org.label}) read the ledger — ${transfers.length} transfers, ${balances.length} balances`);
  } catch (err) {
    failures++;
    bad(`${mspId} (${org.label}) could not read: ${err.message}`);
  }
}

closeAll();
process.exit(failures ? 1 : 0);
