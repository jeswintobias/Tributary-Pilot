// Ledger state as ISO 20022 messages.
//
// WHY BOTHER
//
// Everything this project settles is already a payment, and the rest of the
// industry has one way of describing payments. A corridor that cannot emit a
// pacs.008 is a corridor that needs a translation layer written by whoever
// adopts it, and the translation is where meaning gets lost. Emitting the
// standard shapes is how the work stays legible to a payments operator who
// has never read a line of chaincode.
//
// THE MAPPING
//
//   CAPTURED   pacs.008  the instruction, as sent
//   ACCEPTED   pacs.002  ACTC  — accepted, technically validated
//   PREPARED   pacs.002  PDNG  — pending: both legs reserved, nothing moved
//   SETTLED    pacs.002  ACSC  — accepted, settlement completed
//   ABORTED    pacs.002  RJCT  — rejected, with a reason
//   ABORTED    pacs.004  the return, when value had been reserved
//
// PREPARED is the interesting one. ISO has no status for "both sides have
// irrevocably committed but neither has moved", because the standard grew up
// around rails where that state does not exist. PDNG is the closest honest
// answer and it understates what the ledger knows — a prepared transfer is a
// great deal more certain than a pending one. That gap is the two-phase commit
// showing through the standard, and it is recorded rather than papered over.
//
// WHAT IS DELIBERATELY EMPTY
//
// pacs.008 has rich debtor and creditor structures: name, postal address,
// date and place of birth. This ledger holds none of it and must not. The
// mapping puts the salted reference in a proprietary identifier and leaves
// every personal field out, which produces a message that is structurally
// valid and commercially incomplete.
//
// That is the correct output for a message built FROM THE LEDGER. A real
// sender would populate those fields from its own compliance vault on the way
// out, never from the chain. Anything else would mean the names were on the
// chain, which is the one thing this project will not do.

import { el, text, indent, DECLARATION } from './xml.js';
import { toDecimal } from './amount.js';
import { rateToDecimal } from './rate.js';
import crypto from 'node:crypto';

const NS_PACS008 = 'urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08';
const NS_PACS002 = 'urn:iso:std:iso:20022:tech:xsd:pacs.002.001.10';
const NS_PACS004 = 'urn:iso:std:iso:20022:tech:xsd:pacs.004.001.09';

/** The ledger's states, mapped to ISO external status codes. */
export const STATUS_FOR_STATE = {
  CAPTURED: 'RCVD',
  ACCEPTED: 'ACTC',
  PREPARED: 'PDNG',
  SETTLED: 'ACSC',
  ABORTED: 'RJCT',
};

/**
 * A UETR for a transfer: a UUID that is the same every time.
 *
 * ISO wants a UUIDv4 and a UUIDv4 is random, which would mean the same
 * transfer produced a different tracker on every call — so two systems
 * regenerating the message would disagree about which payment it was. Derived
 * from the transfer ID instead: still a well-formed UUID, and stable.
 */
export function uetrFor(transferID) {
  const h = crypto.createHash('sha256').update(`tributary-uetr:${transferID}`).digest('hex');
  const v = `${h.slice(0, 8)}-${h.slice(8, 12)}-4${h.slice(13, 16)}-`
    + `${((parseInt(h[16], 16) & 0x3) | 0x8).toString(16)}${h.slice(17, 20)}-${h.slice(20, 32)}`;
  return v;
}

const iso = (unixSeconds) => new Date(Number(unixSeconds) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
const isoDate = (unixSeconds) => new Date(Number(unixSeconds) * 1000).toISOString().slice(0, 10);

/** An institution, identified by MSP ID because that is what the ledger has. */
function agent(tag, mspID) {
  return el(tag, el('FinInstnId', el('Othr', text('Id', mspID))));
}

/**
 * A party, carrying only the opaque reference.
 *
 * `Nm` and `PstlAdr` are absent on purpose, not forgotten. See the note at the
 * top of this file.
 */
function party(tag, reference) {
  return el(tag, el('Id', el('PrvtId', el('Othr',
    text('Id', reference)
    + el('SchmeNm', text('Prtry', 'TRIBUTARY-SALTED-REF'))))));
}

/**
 * pacs.008 — FIToFICustomerCreditTransfer.
 *
 * The instruction. Emitted once, from the captured transfer.
 */
export function pacs008(transfer, { createdAt = transfer.createdAt } = {}) {
  const uetr = uetrFor(transfer.transferID);
  const rate = transfer.quote?.rateNum
    ? rateToDecimal(transfer.quote.rateNum, transfer.quote.rateDen)
    : null;

  const body = el('FIToFICstmrCdtTrf',
    el('GrpHdr',
      text('MsgId', `TRB-${transfer.transferID}`)
      + text('CreDtTm', iso(createdAt))
      + text('NbOfTxs', '1')
      + text('CtrlSum', toDecimal(String(transfer.payoutLegAmount), transfer.payoutCcy))
      + el('SttlmInf', text('SttlmMtd', 'CLRG')))
    + el('CdtTrfTxInf',
      el('PmtId',
        text('InstrId', transfer.transferID)
        + text('EndToEndId', transfer.transferID)
        + text('TxId', transfer.transferID)
        + text('UETR', uetr))
      // The amount that settles between the institutions is the payout leg:
      // what the beneficiary's bank must deliver.
      + el('IntrBkSttlmAmt',
        toDecimal(String(transfer.payoutLegAmount), transfer.payoutCcy),
        { Ccy: transfer.payoutCcy })
      + text('IntrBkSttlmDt', isoDate(createdAt))
      // The amount the sender instructed, in the sending currency.
      + el('InstdAmt',
        toDecimal(String(transfer.senderLegAmount), transfer.senderCcy),
        { Ccy: transfer.senderCcy })
      + (rate ? text('XchgRate', rate.value) : '')
      // SHAR: charges shared. The ledger models no charges at all, so this
      // states the convention rather than reporting a fact.
      + text('ChrgBr', 'SHAR')
      + agent('InstgAgt', transfer.senderOrg)
      + agent('InstdAgt', transfer.payoutOrg)
      + party('Dbtr', `sender-of:${transfer.transferID}`)
      + party('Cdtr', transfer.payoutPayeeRef)
      + (transfer.quote?.quoteID
        ? el('RmtInf', text('Ustrd', `quote ${transfer.quote.quoteID} oracle ${transfer.quote.oracleID}`))
        : '')));

  return {
    messageId: `TRB-${transfer.transferID}`,
    uetr,
    rateExact: rate ? rate.exact : null,
    xml: indent(`${DECLARATION}${el('Document', body, { xmlns: NS_PACS008 })}`),
  };
}

/**
 * pacs.002 — FIToFIPaymentStatusReport.
 *
 * One per state change, which is how a counterparty follows a transfer
 * without reading the ledger.
 */
export function pacs002(transfer, { at = transfer.resolvedAt || transfer.createdAt, reason = '' } = {}) {
  const status = STATUS_FOR_STATE[transfer.state];
  if (!status) throw new Error(`no ISO status mapped for ledger state ${JSON.stringify(transfer.state)}`);

  const reasonBlock = status === 'RJCT'
    ? el('StsRsnInf',
      el('Rsn', text('Cd', 'NARR'))
      + text('AddtlInf', reason || 'settlement window expired; both legs refunded'))
    : (reason ? el('StsRsnInf', text('AddtlInf', reason)) : '');

  const body = el('FIToFIPmtStsRpt',
    el('GrpHdr',
      text('MsgId', `TRB-STS-${transfer.transferID}-${status}`)
      + text('CreDtTm', iso(at)))
    + el('TxInfAndSts',
      text('OrgnlEndToEndId', transfer.transferID)
      + text('OrgnlTxId', transfer.transferID)
      + text('OrgnlUETR', uetrFor(transfer.transferID))
      + text('TxSts', status)
      + reasonBlock
      + agent('InstgAgt', transfer.senderOrg)
      + agent('InstdAgt', transfer.payoutOrg)));

  return {
    messageId: `TRB-STS-${transfer.transferID}-${status}`,
    status,
    ledgerState: transfer.state,
    xml: indent(`${DECLARATION}${el('Document', body, { xmlns: NS_PACS002 })}`),
  };
}

/**
 * pacs.004 — PaymentReturn.
 *
 * An aborted transfer that had reached PREPARED moved value into escrow and
 * moved it back. That is a return, and saying so with a status report alone
 * would lose the fact that funds were reserved and released.
 */
export function pacs004(transfer, { at = transfer.resolvedAt, reason = 'settlement window expired' } = {}) {
  if (transfer.state !== 'ABORTED') {
    throw new Error(`pacs.004 describes a return; transfer ${transfer.transferID} is ${transfer.state}`);
  }

  const body = el('PmtRtr',
    el('GrpHdr',
      text('MsgId', `TRB-RTN-${transfer.transferID}`)
      + text('CreDtTm', iso(at))
      + text('NbOfTxs', '1')
      + el('SttlmInf', text('SttlmMtd', 'CLRG')))
    + el('TxInf',
      text('RtrId', `TRB-RTN-${transfer.transferID}`)
      + text('OrgnlEndToEndId', transfer.transferID)
      + text('OrgnlTxId', transfer.transferID)
      + text('OrgnlUETR', uetrFor(transfer.transferID))
      + el('OrgnlIntrBkSttlmAmt',
        toDecimal(String(transfer.payoutLegAmount), transfer.payoutCcy),
        { Ccy: transfer.payoutCcy })
      + el('RtrdIntrBkSttlmAmt',
        toDecimal(String(transfer.payoutLegAmount), transfer.payoutCcy),
        { Ccy: transfer.payoutCcy })
      + el('RtrRsnInf', el('Rsn', text('Cd', 'NARR')) + text('AddtlInf', reason))));

  return {
    messageId: `TRB-RTN-${transfer.transferID}`,
    xml: indent(`${DECLARATION}${el('Document', body, { xmlns: NS_PACS004 })}`),
  };
}

/**
 * Every message a transfer's current state warrants.
 *
 * A transfer that settled produces its instruction and its completion; one
 * that aborted after reserving produces a return as well.
 */
export function messagesFor(transfer) {
  const out = [{ kind: 'pacs.008', ...pacs008(transfer) }];
  out.push({ kind: 'pacs.002', ...pacs002(transfer) });
  if (transfer.state === 'ABORTED' && Number(transfer.preparedAt) > 0) {
    out.push({ kind: 'pacs.004', ...pacs004(transfer) });
  }
  return out;
}
