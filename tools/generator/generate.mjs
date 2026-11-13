// Regenerates corpus entries on a local, single-node stellar-core started with a chosen network
// passphrase, so the corpus can be reproduced against both the testnet and the mainnet passphrase
// without touching either network.
//
// Usage:
//   node tools/generator/generate.mjs --network testnet|mainnet [--entries cap40,muxed] [--out DIR]
//   node tools/generator/generate.mjs --self-test
//
// --network   starts a single-validator stellar-core in Docker (image pinned by digest below) with
//             RUN_STANDALONE under that network's passphrase, upgrades the ledger to protocol 30,
//             funds fresh accounts from the passphrase's root account, submits each entry's
//             transaction over core's HTTP `tx` command, and takes the envelope, result,
//             TransactionMeta and LedgerCloseMeta of the ledger that applied it from the
//             METADATA_OUTPUT_STREAM that core writes. Output: <out>/<network>/<entry-id>/.
//             Ledgers close on core's own real-time schedule (about every 5 s). MANUAL_CLOSE is not
//             used: with RUN_STANDALONE it switches core to a virtual clock whose close times run
//             ahead of wall time, and stellar-core 30.0.0 aborted on `t >= mVirtualNow`
//             (util/Timer.cpp:287) during a manualclose in our runs.
// --self-test runs the same extraction on the committed testnet LedgerCloseMeta files and checks
//             that it finds each transaction and gives back the committed envelope and result
//             byte-identical (no Docker).
//
// Keys: every account key is generated in memory with Keypair.random() and never printed or
// written. The root account is derived from the passphrase (public knowledge on a local network).
// The validator's NODE_SEED is generated inside the container by `stellar-core gen-seed` and only
// ever lives in the container's tmpfs; the container is removed when the run ends.
// Security: core's HTTP port is published on 127.0.0.1 only, the container has memory, CPU and
// process limits, and it is always removed in a finally block.
import { execFileSync, spawnSync } from "node:child_process";
import { get as httpGet } from "node:http";
import { createHash, randomBytes } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { resolve, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import * as sdk from "@stellar/stellar-sdk";

const { Keypair, TransactionBuilder, Operation, Networks, Asset, MuxedAccount, StrKey, Account } = sdk;

export const IMAGE =
  "stellar/stellar-core:30.0.0-3642.0e49751aa.noble@sha256:eb7a7c81a29b8665c8878d55e1b3979f0d7527426350c4ff258a9c7647ae5c51";
const PROTOCOL = 30;
// Testnet and mainnet both run with a 0.5 XLM base reserve; a fresh standalone genesis has 10 XLM.
const BASE_RESERVE = 5000000;
const NETWORKS = { testnet: Networks.TESTNET, mainnet: Networks.PUBLIC };
const ROOT = new URL("../../", import.meta.url);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const sha = (b64) => createHash("sha256").update(Buffer.from(b64, "base64")).digest("hex");
const hex = (h) => Buffer.from(h.value ?? h).toString("hex");

// ---------- meta stream and extraction ----------

// METADATA_OUTPUT_STREAM is a sequence of XDR records with RFC 5531 record marking: a 4-byte
// big-endian word whose high bit marks the last fragment and whose low 31 bits are the length.
function* frames(buf) {
  let off = 0;
  let parts = [];
  while (off + 4 <= buf.length) {
    const word = buf.readUInt32BE(off);
    const len = word & 0x7fffffff;
    parts.push(buf.subarray(off + 4, off + 4 + len));
    off += 4 + len;
    if (word & 0x80000000) {
      yield Buffer.concat(parts);
      parts = [];
    }
  }
  if (off !== buf.length || parts.length) throw new Error("truncated meta stream");
}

function envelopesIn(node, out = []) {
  if (node === null || typeof node !== "object" || node instanceof Uint8Array) return out;
  if (typeof node.type === "string" && node.type.startsWith("envelopeType")) {
    out.push(node);
    return out;
  }
  for (const v of Array.isArray(node) ? node : Object.values(node)) envelopesIn(v, out);
  return out;
}

// Finds txHash in one LedgerCloseMeta (base64) and returns the four corpus files for it.
export function extract(lcmB64, txHash, passphrase) {
  const lcm = sdk.xdr.LedgerCloseMeta.fromXDR(lcmB64, "base64");
  if (!lcm.v2) throw new Error(`LedgerCloseMeta is ${lcm.type}, not v2`);
  const v2 = lcm.v2;
  const proc = v2.txProcessing.find((p) => hex(p.result.transactionHash) === txHash);
  if (!proc) return null;
  const env = envelopesIn(v2.txSet).find(
    (e) => hex(TransactionBuilder.fromXDR(e.toXDR("base64"), passphrase).hash()) === txHash);
  if (!env) throw new Error(`tx ${txHash} applied but its envelope is not in the tx set`);
  return {
    "envelope.b64": ["TransactionEnvelope", env.toXDR("base64")],
    "result.b64": ["TransactionResult", proc.result.result.toXDR("base64")],
    "meta.b64": ["TransactionMeta", proc.txApplyProcessing.toXDR("base64")],
    "ledger-close-meta.b64": ["LedgerCloseMeta", lcmB64],
    ledgerSeq: v2.ledgerHeader.header.ledgerSeq,
    closeTime: v2.ledgerHeader.header.scpValue.closeTime.toString(),
  };
}

// The committed meta.b64 came from RPC getTransaction and ledger-close-meta.b64 from RPC getLedgers,
// so the two copies of a transaction's meta can differ where core's output is not deterministic
// across nodes (diagnostic core_metrics timings, order of changes inside an operation). Envelope
// and result must match exactly; a meta difference is reported, not failed.
function selfTest() {
  let failed = 0;
  const manifest = JSON.parse(readFileSync(new URL("corpus/manifest.json", ROOT), "utf8"));
  for (const e of manifest.entries) {
    const dir = new URL(`corpus/entries/${e.id}/`, ROOT);
    const read = (f) => readFileSync(new URL(f, dir), "utf8").trim();
    const got = extract(read("ledger-close-meta.b64"), e.source.txHash, e.network.passphrase);
    const bad = got ? ["envelope.b64", "result.b64"].filter((f) => got[f][1] !== read(f)) : ["tx not found"];
    const metaNote = got && got["meta.b64"][1] !== read("meta.b64")
      ? " (meta in the LedgerCloseMeta differs from the committed meta.b64)" : "";
    console.log(`${bad.length ? "FAIL" : "ok  "} ${e.id}${bad.length ? ": " + bad.join(", ") : ""}${metaNote}`);
    failed += bad.length ? 1 : 0;
  }
  if (failed) process.exit(1);
  console.log(`extraction finds the tx and reproduces envelope and result of all ${manifest.entries.length} committed entries`);
}

// ---------- container ----------

const docker = (args, opts = {}) => execFileSync("docker", args, { maxBuffer: 1 << 30, ...opts });

// Runs inside the container. The seed goes straight from gen-seed into the config on tmpfs.
// EMIT_CLASSIC_EVENTS=true because the committed testnet metas carry the classic SAC events
// (fee, transfer) that the testnet RPC's core emits; without it the meta shape would differ.
export const CORE_SCRIPT = `set -eu
SEED=$(stellar-core gen-seed | awk '/^Secret seed:/{print $3}')
umask 077
cat > /data/core.cfg <<EOF
NETWORK_PASSPHRASE="$XDRGEN_PASSPHRASE"
RUN_STANDALONE=true
NODE_IS_VALIDATOR=true
NODE_SEED="$SEED self"
UNSAFE_QUORUM=true
FAILURE_SAFETY=0
HTTP_PORT=11626
PUBLIC_HTTP_PORT=true
DATABASE="sqlite3:///data/stellar.db"
BUCKET_DIR_PATH="/data/buckets"
METADATA_OUTPUT_STREAM="/data/meta.xdr"
METADATA_DEBUG_LEDGERS=0
EMIT_CLASSIC_EVENTS=true
LOG_FILE_PATH=""
[QUORUM_SET]
THRESHOLD_PERCENT=100
VALIDATORS=["\\$self"]
EOF
unset SEED
stellar-core --conf /data/core.cfg new-db >/dev/null
exec stellar-core --conf /data/core.cfg --console run
`;

function startCore(name, passphrase) {
  docker(["run", "-d", "--name", name,
    "--memory", "2g", "--memory-swap", "2g", "--cpus", "2", "--pids-limit", "512",
    "--tmpfs", "/data:rw,size=768m", "-p", "127.0.0.1::11626",
    "-e", `XDRGEN_PASSPHRASE=${passphrase}`,
    "--entrypoint", "/bin/sh", IMAGE, "-c", CORE_SCRIPT], { stdio: ["ignore", "ignore", "inherit"] });
  const port = docker(["port", name, "11626/tcp"]).toString().trim().split("\n")
    .map((l) => l.trim()).find((l) => l.startsWith("127.0.0.1:"));
  if (!port) throw new Error("core HTTP port is not published on 127.0.0.1");
  return { name, base: `http://${port}` };
}

function coreLogTail(name) {
  const r = spawnSync("docker", ["logs", "--tail", "40", name], { encoding: "utf8" });
  return `${r.stdout ?? ""}${r.stderr ?? ""}`.replace(/S[A-Z2-7]{55}/g, "<secret redacted>") || "(no logs)";
}

// node:http with agent:false opens a fresh connection per command: core's HTTP server closes idle
// connections, and a reused keep-alive socket can fail after the command was already sent.
function http(core, path) {
  return new Promise((ok, fail) => {
    const req = httpGet(`${core.base}/${path}`, { agent: false }, (res) => {
      let text = "";
      res.setEncoding("utf8");
      res.on("data", (c) => (text += c));
      res.on("end", () => {
        if (res.statusCode !== 200) return fail(new Error(`core ${path.split("?")[0]}: HTTP ${res.statusCode} ${text}`));
        try {
          ok(JSON.parse(text));
        } catch {
          ok({ text }); // some commands (upgrades) answer with plain text
        }
      });
    });
    req.setTimeout(30000, () => req.destroy(new Error(`core ${path.split("?")[0]}: timeout`)));
    req.on("error", fail);
  });
}

async function waitSynced(core) {
  for (let i = 0; i < 90; i++) {
    try {
      const { info } = await http(core, "info");
      if (info.state === "Synced!") return info;
    } catch { /* not listening yet */ }
    await sleep(1000);
  }
  throw new Error(`core did not reach Synced!\n${coreLogTail(core.name)}`);
}

const ledger = async (core) => (await http(core, "info")).info.ledger;

// Waits until core has closed a ledger with sequence > seq; returns the new last closed ledger.
async function waitLedgerAfter(core, seq) {
  for (let i = 0; i < 150; i++) {
    const now = await ledger(core);
    if (now.num > seq) return now;
    await sleep(200);
  }
  throw new Error(`no ledger closed after ${seq} within 30 s`);
}

async function upgradeProtocol(core) {
  const l = await ledger(core);
  if (l.version < PROTOCOL) {
    await http(core, `upgrades?mode=set&upgradetime=1970-01-01T00:00:00Z&protocolversion=${PROTOCOL}&basereserve=${BASE_RESERVE}`);
    for (let seq = l.num; (await ledger(core)).version < PROTOCOL && seq < l.num + 5;) seq = (await waitLedgerAfter(core, seq)).num;
  }
  const after = await ledger(core);
  if (after.version !== PROTOCOL) throw new Error(`ledger is at protocol ${after.version}, wanted ${PROTOCOL}`);
  return after;
}

// ---------- submission ----------

// Every LedgerCloseMeta core has written so far, by ledger sequence, as base64.
function readMetaStream(core) {
  const byLedger = new Map();
  for (const f of frames(docker(["exec", core.name, "cat", "/data/meta.xdr"]))) {
    const b64 = f.toString("base64");
    const lcm = sdk.xdr.LedgerCloseMeta.fromXDR(b64, "base64");
    byLedger.set((lcm.v2 ?? lcm.v1 ?? lcm.v0).ledgerHeader.header.ledgerSeq, b64); // v0/v1 before the upgrade
  }
  return byLedger;
}

// Submits over core's HTTP `tx` command and waits for the ledger that applies it.
async function submit(core, tx, passphrase) {
  const hash = hex(tx.hash());
  const from = (await ledger(core)).num;
  const r = await http(core, `tx?blob=${encodeURIComponent(tx.toXDR())}`);
  if (r.status !== "PENDING") {
    const detail = r.error ? JSON.stringify(sdk.xdr.TransactionResult.fromXDR(r.error, "base64").toJSON()) : JSON.stringify(r);
    throw new Error(`core rejected the transaction: ${r.status} ${detail}`);
  }
  let seq = from;
  for (let tries = 0; tries < 5; tries++) {
    seq = (await waitLedgerAfter(core, seq)).num;
    for (const [n, b64] of readMetaStream(core)) {
      if (n <= from) continue;
      const got = extract(b64, hash, passphrase);
      if (!got) continue;
      const res = sdk.xdr.TransactionResult.fromXDR(got["result.b64"][1], "base64").toJSON();
      if (!JSON.stringify(res.result).startsWith('{"tx_success"')) throw new Error(`tx ${hash} failed: ${JSON.stringify(res)}`);
      return { hash, ledger: n };
    }
  }
  throw new Error(`tx ${hash} was accepted but not applied within 5 ledgers`);
}

function makeNet(core, passphrase) {
  const root = Keypair.master(passphrase);
  let rootSeq = 0n; // a fresh genesis root account starts at sequence 0
  const net = { core, passphrase };
  net.build = (sourceAccount, signers, ops) => {
    const b = new TransactionBuilder(sourceAccount, { fee: "1000", networkPassphrase: passphrase })
      .setTimeout(120); // as tools/capture-testnet.mjs
    ops.forEach((o) => b.addOperation(o));
    const tx = b.build();
    signers.forEach((kp) => tx.sign(kp));
    return tx;
  };
  // Creates fresh accounts funded with 10,000 XLM, as testnet friendbot does. Core starts a new
  // account's sequence number at ledgerSeq << 32 (protocol >= 10).
  net.accounts = async (n) => {
    const kps = Array.from({ length: n }, () => Keypair.random());
    const tx = net.build(new Account(root.publicKey(), rootSeq.toString()), [root],
      kps.map((kp) => Operation.createAccount({ destination: kp.publicKey(), startingBalance: "10000" })));
    const { ledger: seq } = await submit(core, tx, passphrase);
    rootSeq += 1n;
    return kps.map((kp) => ({ kp, seq: (BigInt(seq) << 32n).toString() }));
  };
  return net;
}

// ---------- entries: same transactions as tools/capture-testnet.mjs ----------

// CAP-40: add an ed25519 signed-payload signer with a 64-byte payload through SetOptions.
async function cap40(net) {
  const [a] = await net.accounts(1);
  const payload = Buffer.from("xdr-shape-corpus cap40 sample payload, 64 bytes max ............").subarray(0, 64);
  const signer = StrKey.encodeSignedPayload(Buffer.concat([
    StrKey.decodeEd25519PublicKey(a.kp.publicKey()),
    Buffer.from([0, 0, 0, payload.length]),
    payload,
    Buffer.alloc((4 - (payload.length % 4)) % 4),
  ]));
  const tx = net.build(new Account(a.kp.publicKey(), a.seq), [a.kp],
    [Operation.setOptions({ signer: { ed25519SignedPayload: signer, weight: 1 } })]);
  return { id: "cap40-signed-payload-signer-max-payload", got: await submit(net.core, tx, net.passphrase),
    meta: { signedPayloadSigner: signer, payloadLength: payload.length } };
}

// CAP-27: payment from a muxed source with id u64 max to a muxed destination with id 0.
async function muxed(net) {
  const [a, b] = await net.accounts(2);
  const MAX = "18446744073709551615";
  const dest = new MuxedAccount(new Account(b.kp.publicKey(), "0"), "0").accountId();
  const src = new MuxedAccount(new Account(a.kp.publicKey(), a.seq), MAX);
  const tx = net.build(src, [a.kp], [Operation.payment({ destination: dest, asset: Asset.native(), amount: "1" })]);
  return { id: "cap27-muxed-source-and-destination-id-extremes", got: await submit(net.core, tx, net.passphrase),
    meta: { muxedSource: new MuxedAccount(new Account(a.kp.publicKey(), "0"), MAX).accountId(), muxedSourceId: MAX,
      muxedDestination: dest, muxedDestinationId: "0" } };
}

const TASKS = { cap40, muxed };

// ---------- output ----------

function save(outDir, network, passphrase, coreVersion, run, lcmByLedger) {
  const lcm = lcmByLedger.get(run.got.ledger);
  if (!lcm) throw new Error(`${run.id}: no LedgerCloseMeta for ledger ${run.got.ledger} in the meta stream`);
  const files = extract(lcm, run.got.hash, passphrase);
  if (!files) throw new Error(`${run.id}: tx ${run.got.hash} is not in ledger ${run.got.ledger}`);
  const dir = new URL(`${network}/${run.id}/`, outDir);
  mkdirSync(dir, { recursive: true });
  const list = [];
  for (const name of ["envelope.b64", "result.b64", "meta.b64", "ledger-close-meta.b64"]) {
    const [xdrType, b64] = files[name];
    writeFileSync(new URL(name, dir), b64 + "\n");
    list.push({ path: `${network}/${run.id}/${name}`, xdrType, encoding: "base64", sha256: sha(b64) });
  }
  writeFileSync(new URL("capture.json", dir), JSON.stringify({
    id: run.id, txHash: run.got.hash, ledger: run.got.ledger, ledgerCloseTime: files.closeTime,
    capturedAt: new Date().toISOString(),
    capturedVia: "local single-validator stellar-core (RUN_STANDALONE), HTTP tx, METADATA_OUTPUT_STREAM",
    generator: "tools/generator/generate.mjs", image: IMAGE, coreVersion,
    network: { name: `local-${network}-passphrase`, passphrase }, files: list, ...run.meta,
  }, null, 2) + "\n");
  console.log(`${network} ${run.id}: tx ${run.got.hash} ledger ${run.got.ledger}`);
}

async function generate(network, entries, outDir) {
  const passphrase = NETWORKS[network];
  if (!passphrase) throw new Error(`--network must be one of ${Object.keys(NETWORKS).join(", ")}`);
  for (const e of entries) if (!TASKS[e]) throw new Error(`unknown entry ${e}; known: ${Object.keys(TASKS).join(", ")}`);
  const name = `xdr-gen-${network}-${randomBytes(4).toString("hex")}`;
  let core = { name };
  try {
    core = startCore(name, passphrase);
    const info = await waitSynced(core);
    const upgraded = await upgradeProtocol(core);
    console.log(`${network}: ${info.build}, passphrase "${passphrase}", ledger ${upgraded.num} at protocol ${upgraded.version}`);
    const net = makeNet(core, passphrase);
    const runs = [];
    for (const e of entries) runs.push(await TASKS[e](net));
    const lcmByLedger = readMetaStream(core);
    for (const run of runs) save(outDir, network, passphrase, info.build, run, lcmByLedger);
  } catch (err) {
    console.error(coreLogTail(core.name));
    throw err;
  } finally {
    spawnSync("docker", ["rm", "-f", name], { stdio: "ignore" });
  }
}

function arg(name) {
  const i = process.argv.indexOf(name);
  return i >= 0 ? process.argv[i + 1] : undefined;
}

if (!process.argv[1] || import.meta.url !== pathToFileURL(resolve(process.argv[1])).href) {
  // imported as a module: nothing to run
} else if (process.argv.includes("--self-test")) {
  selfTest();
} else {
  const out = pathToFileURL(resolve(fileURLToPath(ROOT), arg("--out") ?? "tools/generator/out") + sep);
  await generate(arg("--network"), (arg("--entries") ?? Object.keys(TASKS).join(",")).split(","), out);
}
