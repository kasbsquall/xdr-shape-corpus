// Captures core-emitted XDR for corpus entries from Stellar testnet.
// Keys are ephemeral, generated in memory, funded by friendbot and never printed or stored.
// Usage: node capture-testnet.mjs <entry> [<entry> ...]   entries: cap40, muxed, sac, muxedSac
import { writeFileSync, mkdirSync } from "node:fs";
import { createHash } from "node:crypto";
import * as sdk from "@stellar/stellar-sdk";

const { Keypair, TransactionBuilder, Operation, Networks, Asset, MuxedAccount, Horizon, StrKey, Account } = sdk;
const RPC = "https://soroban-testnet.stellar.org";
const HORIZON = "https://horizon-testnet.stellar.org";
const PASSPHRASE = Networks.TESTNET;
const horizon = new Horizon.Server(HORIZON);
const OUT = new URL("../corpus/entries/", import.meta.url);

async function rpc(method, params) {
  const res = await fetch(RPC, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  });
  const body = await res.json();
  if (body.error) throw new Error(`${method}: ${JSON.stringify(body.error)}`);
  return body.result;
}

async function fund(kp) {
  const res = await fetch(`https://friendbot.stellar.org?addr=${kp.publicKey()}`);
  if (!res.ok) throw new Error(`friendbot ${res.status}`);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function submit(tx) {
  const sent = await rpc("sendTransaction", { transaction: tx.toXDR() });
  if (sent.status !== "PENDING") throw new Error(`sendTransaction: ${JSON.stringify(sent)}`);
  for (let i = 0; i < 30; i++) {
    await sleep(2000);
    const got = await rpc("getTransaction", { hash: sent.hash });
    if (got.status === "SUCCESS") return { hash: sent.hash, ...got };
    if (got.status === "FAILED") throw new Error(`tx failed: ${JSON.stringify(got)}`);
  }
  throw new Error("timeout waiting for tx");
}

async function ledgerCloseMeta(seq) {
  const r = await rpc("getLedgers", { startLedger: seq, pagination: { limit: 1 } });
  const l = r.ledgers.find((x) => x.sequence === seq);
  if (!l) throw new Error(`ledger ${seq} not returned`);
  return l.metadataXdr;
}

const sha = (b64) => createHash("sha256").update(Buffer.from(b64, "base64")).digest("hex");

function save(id, got, lcm, meta) {
  const dir = new URL(`${id}/`, OUT);
  mkdirSync(dir, { recursive: true });
  const files = {
    "envelope.b64": ["TransactionEnvelope", got.envelopeXdr],
    "result.b64": ["TransactionResult", got.resultXdr],
    "meta.b64": ["TransactionMeta", got.resultMetaXdr],
    "ledger-close-meta.b64": ["LedgerCloseMeta", lcm],
  };
  const list = [];
  for (const [name, [xdrType, b64]] of Object.entries(files)) {
    writeFileSync(new URL(name, dir), b64 + "\n");
    list.push({ path: `corpus/entries/${id}/${name}`, xdrType, encoding: "base64", sha256: sha(b64) });
  }
  writeFileSync(new URL("capture.json", dir), JSON.stringify({
    id, txHash: got.hash, ledger: got.ledger, ledgerCloseTime: got.createdAt,
    capturedAt: new Date().toISOString(), capturedVia: "stellar-rpc sendTransaction/getTransaction/getLedgers",
    rpc: RPC, passphrase: PASSPHRASE, files: list, ...meta,
  }, null, 2) + "\n");
  console.log(`${id}: tx ${got.hash} ledger ${got.ledger}`);
}

async function newAccount() {
  const kp = Keypair.random();
  await fund(kp);
  await sleep(1500);
  return kp;
}

async function build(sourceKp, ops, sourceOverride) {
  const acc = await horizon.loadAccount(sourceKp.publicKey());
  const src = sourceOverride ? sourceOverride(acc) : acc;
  const b = new TransactionBuilder(src, { fee: "1000", networkPassphrase: PASSPHRASE }).setTimeout(120);
  ops.forEach((o) => b.addOperation(o));
  const tx = b.build();
  tx.sign(sourceKp);
  return tx;
}

// CAP-40: add an ed25519 signed-payload signer through SetOptions.
async function cap40() {
  const a = await newAccount();
  const payload = Buffer.from("xdr-shape-corpus cap40 sample payload, 64 bytes max ............").subarray(0, 64);
  const signer = StrKey.encodeSignedPayload(
    Buffer.concat([
      StrKey.decodeEd25519PublicKey(a.publicKey()),
      Buffer.from([0, 0, 0, payload.length]),
      payload,
      Buffer.alloc((4 - (payload.length % 4)) % 4),
    ]),
  );
  const tx = await build(a, [Operation.setOptions({ signer: { ed25519SignedPayload: signer, weight: 1 } })]);
  const got = await submit(tx);
  save("cap40-signed-payload-signer-max-payload", got, await ledgerCloseMeta(got.ledger), {
    signedPayloadSigner: signer, payloadLength: payload.length,
  });
}

// CAP-27: payment from a muxed source to a muxed destination, with the maximum and minimum muxed IDs.
async function muxed() {
  const [a, b] = [await newAccount(), await newAccount()];
  const MAX = "18446744073709551615";
  const dest = new MuxedAccount(new Account(b.publicKey(), "0"), "0").accountId();
  const tx = await build(
    a,
    [Operation.payment({ destination: dest, asset: Asset.native(), amount: "1" })],
    (acc) => new MuxedAccount(acc, MAX),
  );
  const got = await submit(tx);
  save("cap27-muxed-source-and-destination-id-extremes", got, await ledgerCloseMeta(got.ledger), {
    muxedSource: new MuxedAccount(new Account(a.publicKey(), "0"), MAX).accountId(), muxedSourceId: MAX,
    muxedDestination: dest, muxedDestinationId: "0",
  });
}

// CAP-73: a trustline created by the Stellar Asset Contract `trust(address)` call instead of ChangeTrust.
async function sac() {
  const server = new sdk.rpc.Server(RPC);
  const [issuer, holder] = [await newAccount(), await newAccount()];
  const asset = new Asset("XDRK", issuer.publicKey());
  const prepared = async (kp, op) => {
    const acc = await server.getAccount(kp.publicKey());
    const raw = new TransactionBuilder(acc, { fee: "1000", networkPassphrase: PASSPHRASE }).setTimeout(120).addOperation(op).build();
    const tx = await server.prepareTransaction(raw);
    tx.sign(kp);
    return tx;
  };
  await submit(await prepared(issuer, Operation.createStellarAssetContract({ asset })));
  const contractId = asset.contractId(PASSPHRASE);
  const got = await submit(await prepared(holder, Operation.invokeContractFunction({
    contract: contractId,
    function: "trust",
    args: [new sdk.Address(holder.publicKey()).toScVal()],
  })));
  save("cap73-sac-created-trustline", got, await ledgerCloseMeta(got.ledger), {
    asset: `XDRK:${issuer.publicKey()}`, sacContractId: contractId, trustor: holder.publicKey(),
  });
}

// Muxed address in a Soroban call: native SAC transfer() to an M address with the maximum muxed ID,
// sent from a muxed transaction source when the network accepts it.
async function muxedSac() {
  const server = new sdk.rpc.Server(RPC);
  const [from, to] = [await newAccount(), await newAccount()];
  const MAX = "18446744073709551615";
  const dest = new MuxedAccount(new Account(to.publicKey(), "0"), MAX).accountId();
  const contractId = Asset.native().contractId(PASSPHRASE);
  const op = Operation.invokeContractFunction({
    contract: contractId,
    function: "transfer",
    args: [
      new sdk.Address(from.publicKey()).toScVal(),
      sdk.Address.fromString(dest).toScVal(),
      sdk.nativeToScVal(10000000n, { type: "i128" }),
    ],
  });
  const attempt = async (muxedSource) => {
    const acc = await server.getAccount(from.publicKey());
    const src = muxedSource ? new MuxedAccount(acc, MAX) : acc;
    const raw = new TransactionBuilder(src, { fee: "1000", networkPassphrase: PASSPHRASE }).setTimeout(120).addOperation(op).build();
    const tx = await server.prepareTransaction(raw);
    tx.sign(from);
    return submit(tx);
  };
  let got, muxedSource = true;
  try {
    got = await attempt(true);
  } catch (e) {
    console.log(`muxed transaction source not accepted (${String(e.message).slice(0, 160)}); retrying with a G source`);
    muxedSource = false;
    got = await attempt(false);
  }
  save("cap67-sac-transfer-to-muxed-address-max-id", got, await ledgerCloseMeta(got.ledger), {
    from: from.publicKey(), muxedDestination: dest, muxedDestinationId: MAX, destinationAccount: to.publicKey(),
    muxedTransactionSource: muxedSource ? new MuxedAccount(new Account(from.publicKey(), "0"), MAX).accountId() : null,
    sacContractId: contractId, amountStroops: "10000000",
  });
}

const tasks = { cap40, muxed, sac, muxedSac };
for (const name of process.argv.slice(2)) {
  if (!tasks[name]) throw new Error(`unknown entry ${name}`);
  await tasks[name]();
}
