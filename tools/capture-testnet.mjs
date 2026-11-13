// Captures core-emitted XDR for corpus entries from Stellar testnet.
// Keys are ephemeral, generated in memory, funded by friendbot and never printed or stored.
// Usage: node capture-testnet.mjs <entry> [<entry> ...]
//   entries: cap40, muxed, sac, muxedSac, lpPair, scmapCollision, nestedAuth, maxLength, offer
// scmapCollision and nestedAuth deploy tools/contracts/probe; build it first with
//   cargo build --release --target wasm32v1-none --manifest-path contracts/probe/Cargo.toml
import { writeFileSync, mkdirSync, readFileSync } from "node:fs";
import { createHash, randomBytes } from "node:crypto";
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

async function prepare(server, kp, op) {
  const acc = await server.getAccount(kp.publicKey());
  const raw = new TransactionBuilder(acc, { fee: "1000", networkPassphrase: PASSPHRASE }).setTimeout(120).addOperation(op).build();
  const tx = await server.prepareTransaction(raw);
  tx.sign(kp);
  return tx;
}

const PROBE_WASM = new URL("contracts/probe/target/wasm32v1-none/release/xdr_corpus_probe.wasm", import.meta.url);

// Uploads and instantiates the probe contract; returns its C... address and the wasm hash.
async function deployProbe(server, kp) {
  const wasm = readFileSync(PROBE_WASM);
  const wasmHash = createHash("sha256").update(wasm).digest();
  await submit(await prepare(server, kp, Operation.uploadContractWasm({ wasm })));
  const created = await submit(await prepare(server, kp, Operation.createCustomContract({
    address: new sdk.Address(kp.publicKey()), wasmHash, salt: randomBytes(32),
  })));
  const meta = sdk.xdr.TransactionMeta.fromXDR(created.resultMetaXdr, "base64");
  return { contractId: sdk.Address.fromScVal(meta.v4.sorobanMeta.returnValue).toString(), wasmHash: wasmHash.toString("hex") };
}

// CAP-38: pool-share trustline and deposit for two assets with the same code whose issuers sort one way
// by G... strkey text and the other way by key bytes. Core requires asset A < asset B by XDR bytes.
async function lpPair() {
  const textFirst = (x, y) => x.publicKey() < y.publicKey();
  const bytesFirst = (x, y) => Buffer.compare(x.rawPublicKey(), y.rawPublicKey()) < 0;
  let i1, i2;
  do {
    [i1, i2] = [Keypair.random(), Keypair.random()];
  } while (textFirst(i1, i2) === bytesFirst(i1, i2));
  const [first, second] = bytesFirst(i1, i2) ? [i1, i2] : [i2, i1];
  await fund(first);
  await fund(second);
  const holder = await newAccount();
  const code = "PAIR";
  const [A, B] = [new Asset(code, first.publicKey()), new Asset(code, second.publicKey())];
  await submit(await build(holder, [Operation.changeTrust({ asset: A }), Operation.changeTrust({ asset: B })]));
  const pay = await build(first, [
    Operation.payment({ destination: holder.publicKey(), asset: A, amount: "1000" }),
    Operation.payment({ destination: holder.publicKey(), asset: B, amount: "1000", source: second.publicKey() }),
  ]);
  pay.sign(second);
  await submit(pay);
  const pool = new sdk.LiquidityPoolAsset(A, B, sdk.LiquidityPoolFeeV18);
  const poolId = Buffer.from(sdk.getLiquidityPoolId("constant_product", pool.getLiquidityPoolParameters())).toString("hex");
  const got = await submit(await build(holder, [
    Operation.changeTrust({ asset: pool }),
    Operation.liquidityPoolDeposit({ liquidityPoolId: poolId, maxAmountA: "100", maxAmountB: "200", minPrice: "0.4", maxPrice: "0.6" }),
  ]));
  save("cap38-pool-share-issuers-ordered-by-key-bytes", got, await ledgerCloseMeta(got.ledger), {
    assetCode: code, issuerFirstByBytes: first.publicKey(), issuerSecondByBytes: second.publicKey(),
    liquidityPoolId: poolId, depositor: holder.publicKey(),
  });
}

// Soroban map argument whose keys are distinct ScVals that print the same: u32 1, string "1" and symbol "1".
// The probe contract echoes it, so core emits the map in the arguments, the return value and an event.
async function scmapCollision() {
  const server = new sdk.rpc.Server(RPC);
  const kp = await newAccount();
  const { contractId, wasmHash } = await deployProbe(server, kp);
  const { xdr } = sdk;
  const map = xdr.ScVal.scvMap([ // host order: by ScVal type (u32 < string < symbol), then by value
    new xdr.ScMapEntry({ key: xdr.ScVal.scvU32(1), val: xdr.ScVal.scvU32(10) }),
    new xdr.ScMapEntry({ key: xdr.ScVal.scvString("1"), val: xdr.ScVal.scvU32(20) }),
    new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("1"), val: xdr.ScVal.scvU32(30) }),
  ]);
  const got = await submit(await prepare(server, kp, Operation.invokeContractFunction({
    contract: contractId, function: "echo", args: [map],
  })));
  save("cap46-scmap-keys-collide-after-native-conversion", got, await ledgerCloseMeta(got.ledger), {
    probeContractId: contractId, probeWasmSha256: wasmHash,
  });
}

// Authorization tree with a sub-invocation: `from` signs probe.pay(), which calls the native SAC transfer()
// that also requires `from`. The transaction source is a different account, so the entry carries
// address credentials with a signature instead of source-account credentials.
async function nestedAuth() {
  const server = new sdk.rpc.Server(RPC);
  const [src, from, to] = [await newAccount(), await newAccount(), await newAccount()];
  const { contractId, wasmHash } = await deployProbe(server, src);
  const sac = Asset.native().contractId(PASSPHRASE);
  const args = [
    new sdk.Address(from.publicKey()).toScVal(),
    sdk.Address.fromString(sac).toScVal(),
    new sdk.Address(to.publicKey()).toScVal(),
    sdk.nativeToScVal(10000000n, { type: "i128" }),
  ];
  const acc = await server.getAccount(src.publicKey());
  const sim = await server.simulateTransaction(new TransactionBuilder(acc, { fee: "1000", networkPassphrase: PASSPHRASE })
    .setTimeout(120).addOperation(Operation.invokeContractFunction({ contract: contractId, function: "pay", args })).build());
  if (sdk.rpc.Api.isSimulationError(sim)) throw new Error(`simulate: ${sim.error}`);
  const until = (await server.getLatestLedger()).sequence + 100;
  const auth = await Promise.all(sim.result.auth.map((e) => sdk.authorizeEntry(e, from, until, PASSPHRASE)));
  const got = await submit(await prepare(server, src, Operation.invokeContractFunction({ contract: contractId, function: "pay", args, auth })));
  save("cap46-nested-authorization-contract-calls-sac", got, await ledgerCloseMeta(got.ledger), {
    probeContractId: contractId, probeWasmSha256: wasmHash, sacContractId: sac,
    authorizer: from.publicKey(), recipient: to.publicKey(), transactionSource: src.publicKey(), amountStroops: "10000000",
  });
}

// Var-arrays at their bound in one transaction core accepts: 100 operations (MAX_OPS_PER_TX), each a
// ManageData with a 64-character name (string64) and a 64-byte value (DataValue<64>), a 28-byte text memo
// (string<28>), and 2 extra signers (CAP-21, extraSigners<2>) that are signed-payload signers with 64-byte payloads.
async function maxLength() {
  const a = await newAccount();
  const payloads = [0, 1].map((k) => Buffer.from(Array.from({ length: 64 }, (_, j) => (k * 64 + j) & 0xff)));
  const extraSigners = payloads.map((p) => StrKey.encodeSignedPayload(
    Buffer.concat([a.rawPublicKey(), Buffer.from([0, 0, 0, p.length]), p])));
  const memo = "xdr-shape-corpus max memo 28";
  if (Buffer.byteLength(memo) !== 28) throw new Error("memo must be 28 bytes");
  const dataName = (i) => `xdr-shape-corpus-max-length-op-${String(i).padStart(3, "0")}-`.padEnd(64, "x");
  const ops = Array.from({ length: 100 }, (_, i) => Operation.manageData({
    name: dataName(i), value: Buffer.from(Array.from({ length: 64 }, (_, j) => (i + j) & 0xff)),
  }));
  const acc = await horizon.loadAccount(a.publicKey());
  const b = new TransactionBuilder(acc, { fee: "1000", networkPassphrase: PASSPHRASE, memo: sdk.Memo.text(memo) })
    .setTimeout(120).setExtraSigners(extraSigners);
  ops.forEach((o) => b.addOperation(o));
  const tx = b.build();
  tx.sign(a);
  payloads.forEach((p) => tx.addDecoratedSignature(a.signPayloadDecorated(p)));
  const got = await submit(tx);
  save("cap21-vectors-at-max-length", got, await ledgerCloseMeta(got.ledger), {
    source: a.publicKey(), operationCount: ops.length, memoText: memo,
    extraSigner0: extraSigners[0], extraSigner1: extraSigners[1], lastDataName: dataName(ops.length - 1),
  });
}

// CAP-6: ManageBuyOffer that updates an existing offer by the offer ID core assigned when it was created.
// The network only emits IDs it allocated, so this is an ordinary in-range offer ID.
async function offer() {
  const issuer = await newAccount();
  const asset = new Asset("OFFR", issuer.publicKey());
  const created = await submit(await build(issuer, [
    Operation.manageBuyOffer({ selling: asset, buying: Asset.native(), buyAmount: "10", price: "1", offerId: "0" }),
  ]));
  const res = sdk.xdr.TransactionResult.fromXDR(created.resultXdr, "base64");
  const offerId = res.result.results[0].tr.manageBuyOfferResult.success.offer.offer.offerId.toString();
  const got = await submit(await build(issuer, [
    Operation.manageBuyOffer({ selling: asset, buying: Asset.native(), buyAmount: "20", price: "2", offerId }),
  ]));
  save("cap06-manage-buy-offer-update-by-id", got, await ledgerCloseMeta(got.ledger), {
    seller: issuer.publicKey(), offerId, createdInTx: created.hash, createdInLedger: created.ledger,
  });
}

const tasks = { cap40, muxed, sac, muxedSac, lpPair, scmapCollision, nestedAuth, maxLength, offer };
for (const name of process.argv.slice(2)) {
  if (!tasks[name]) throw new Error(`unknown entry ${name}`);
  await tasks[name]();
}
