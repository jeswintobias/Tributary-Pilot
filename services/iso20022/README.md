# ISO 20022 messages from ledger state

```bash
npm test     # 19 tests, no dependencies

peer chaincode query -C mychannel -n settlement \
  -c '{"Args":["GetTransfer","TP-1"]}' | node src/cli.js

node src/cli.js --kind pacs.008 --out ./messages
```

| Module | What it does |
|---|---|
| `messages.js` | the mapping: ledger state to `pacs.008` / `pacs.002` / `pacs.004` |
| `amount.js` | minor units to the currency's decimal, exactly, with no float |
| `rate.js` | an exact rational to a decimal, flagged when it cannot be exact |
| `xml.js` | enough XML to write four message shapes, with escaping |

No dependencies. The whole job is four message shapes and the hard part is
escaping, which is twelve lines.

Why `PREPARED` maps to a status that understates it, why every personal-data
field is deliberately empty, and what has not been done:
[docs/ISO20022.md](../../docs/ISO20022.md).
