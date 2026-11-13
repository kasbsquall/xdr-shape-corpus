// Conformance runner for js-stellar-sdk.
// Decodes every corpus file and every sighting vector with the SDK installed in <sdk-dir>,
// re-encodes it and compares bytes, then runs the behaviour check of each sighting.
// Usage: node run.mjs <sdk-dir> <out.json>
import { createRequire } from "node:module";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const KIT = resolve(HERE, "..", "..");
const [sdkDir, outPath] = process.argv.slice(2);
if (!sdkDir || !outPath) throw new Error("usage: node run.mjs <sdk-dir> <out.json>");

const sdkRoot = resolve(sdkDir);
const require = createRequire(join(sdkRoot, "package.json"));
const sdk = require("@stellar/stellar-sdk");
const { xdr } = sdk;
const version = JSON.parse(readFileSync(join(sdkRoot, "node_modules/@stellar/stellar-sdk/package.json"), "utf8")).version;

function roundTrip(xdrType, b64) {
  try {
    const value = xdr[xdrType].fromXDR(b64, "base64");
    const again = value.toXDR("base64");
    return { decode: "ok", roundTripIdentical: again === b64 };
  } catch (e) {
    return { decode: "error", error: String(e.message || e).slice(0, 200) };
  }
}

const manifest = JSON.parse(readFileSync(join(KIT, "corpus/manifest.json"), "utf8"));
const corpus = [];
for (const entry of manifest.entries) {
  for (const f of entry.files) {
    const b64 = readFileSync(join(KIT, f.path), "utf8").trim();
    corpus.push({ entry: entry.id, path: f.path, xdrType: f.xdrType, ...roundTrip(f.xdrType, b64) });
  }
}

// Runs fn and returns { value } or, when it throws, { error } with the message.
function attempt(fn) {
  try {
    return { value: fn() };
  } catch (e) {
    return { error: String(e.message || e).slice(0, 100) };
  }
}

// Builds a result from a list of failures: "ok" when there are none.
const verdict = (failures, okDetail) =>
  failures.length ? { result: "defect", detail: failures.join("; ").slice(0, 300) } : { result: "ok", detail: okDetail };

const scValFromJson = (k) => {
  const [type, v] = Object.entries(k)[0];
  if (type === "u32") return xdr.ScVal.scvU32(v);
  if (type === "i32") return xdr.ScVal.scvI32(v);
  if (type === "bytes") return xdr.ScVal.scvBytes(Buffer.from(v, "hex"));
  if (type === "string") return xdr.ScVal.scvString(v);
  if (type === "symbol") return xdr.ScVal.scvSymbol(v);
  throw new Error(`unknown key type ${type}`);
};
const label = (k) => JSON.stringify(k).replace(/[{}"]/g, "").replace(":", " ");

// Behaviour checks, one per check name. Each returns "defect" (the sighting's bug is present),
// "ok" (behaves as the fix requires) or "n/a".
const checks = {
  // js-stellar-sdk#1789: converting a map must keep every entry or throw.
  "native-map-conversion-keeps-every-entry": (s, value) => {
    const r = attempt(() => sdk.scValToNative(value));
    if (r.error) return { result: "ok", detail: `throws: ${r.error}` };
    const kept = Object.keys(r.value).length;
    return {
      result: kept === s.mapEntries ? "ok" : "defect",
      detail: `${s.mapEntries} entries in XDR, ${kept} after scValToNative`,
    };
  },

  // js-stellar-sdk#1766: assets with the same code order by issuer key bytes.
  "asset-pair-ordered-by-issuer-bytes": ({ inputs }) => {
    const first = new sdk.Asset(inputs.code, inputs.issuerFirstByBytes);
    const second = new sdk.Asset(inputs.code, inputs.issuerSecondByBytes);
    const pool = (a, b) => attempt(() => new sdk.LiquidityPoolAsset(a, b, sdk.LiquidityPoolFeeV18));
    const failures = [];
    const ordered = pool(first, second);
    if (ordered.error) failures.push(`LiquidityPoolAsset(first, second) throws: ${ordered.error}`);
    if (!pool(second, first).error) failures.push("LiquidityPoolAsset(second, first) accepted");
    return verdict(failures, "LiquidityPoolAsset accepts (first, second) and throws for (second, first)");
  },

  // js-stellar-sdk#1766: scvSortedMap puts keys in the order the Soroban host requires.
  "scmap-keys-sorted-like-host": ({ inputs }) => {
    const entries = inputs.keys.map((k) => new xdr.ScMapEntry({ key: scValFromJson(k), val: xdr.ScVal.scvVoid() }));
    const got = sdk.scvSortedMap(entries).map.map((e) => e.key.toXDR("base64"));
    const want = inputs.hostOrder.map((k) => scValFromJson(k).toXDR("base64"));
    const i = want.findIndex((w, n) => w !== got[n]);
    if (i === -1) return { result: "ok", detail: `scvSortedMap puts all ${want.length} keys in host order` };
    const at = inputs.keys[entries.findIndex((e) => e.key.toXDR("base64") === got[i])];
    return { result: "defect", detail: `scvSortedMap puts ${label(at)} at position ${i + 1}; host order has ${label(inputs.hostOrder[i])}` };
  },

  // js-stellar-sdk#1785: a muxed account reports and encodes the ID it was given, or throws.
  "muxed-id-encodes-the-id-given": ({ inputs }) => {
    const account = new sdk.Account(inputs.account, "0");
    const failures = [];
    for (const id of inputs.accepted) {
      const want = BigInt(id).toString();
      const r = attempt(() => {
        const m = new sdk.MuxedAccount(account, id);
        return [m.id(), sdk.MuxedAccount.fromAddress(m.accountId(), "0").id()];
      });
      if (r.error) failures.push(`"${id}" throws: ${r.error}`);
      else if (r.value[0] !== want || r.value[1] !== want) failures.push(`"${id}": id() is "${r.value[0]}", M-address encodes ${r.value[1]}`);
    }
    for (const id of inputs.rejected) {
      const r = attempt(() => sdk.MuxedAccount.fromAddress(new sdk.MuxedAccount(account, id).accountId(), "0").id());
      if (!r.error) failures.push(`"${id}" accepted as ID ${r.value}`);
    }
    return verdict(failures, `MuxedAccount keeps ${inputs.accepted.length} IDs and throws for ${inputs.rejected.length}`);
  },

  // js-stellar-sdk#1785: manageSellOffer encodes the offer ID it was given, or throws.
  "offer-id-encodes-the-id-given": ({ inputs }) => {
    const buying = new sdk.Asset("USD", inputs.issuer);
    const encode = (offerId) => attempt(() =>
      sdk.Operation.manageSellOffer({ selling: sdk.Asset.native(), buying, amount: "1", price: "1", offerId })
        .body.manageSellOfferOp.offerId.toString());
    const failures = [];
    for (const id of inputs.accepted) {
      const r = encode(id);
      if (r.error) failures.push(`"${id}" throws: ${r.error}`);
      else if (r.value !== id) failures.push(`"${id}" encodes ${r.value}`);
    }
    for (const id of inputs.rejected) {
      const r = encode(id);
      if (!r.error) failures.push(`"${id}" accepted as offer ${r.value}`);
    }
    for (const id of inputs.rejectedAsFloat) {
      const r = encode(Number(id));
      if (!r.error) failures.push(`number ${id} accepted as offer ${r.value}`);
    }
    const n = inputs.rejected.length + inputs.rejectedAsFloat.length;
    return verdict(failures, `manageSellOffer keeps ${inputs.accepted.length} IDs and throws for ${n}`);
  },
};

const sightings = JSON.parse(readFileSync(join(HERE, "..", "sightings.json"), "utf8")).sightings;
const vectors = [];
for (const s of sightings) {
  if (!s.file) {
    vectors.push({ sighting: s.id, check: s.check, ...checks[s.check](s) });
    continue;
  }
  const b64 = readFileSync(join(HERE, "..", s.file), "utf8").trim();
  const rt = roundTrip(s.xdrType, b64);
  let check = { result: "n/a" };
  if (rt.decode === "ok" && checks[s.check]) {
    check = checks[s.check](s, xdr[s.xdrType].fromXDR(b64, "base64"));
  }
  vectors.push({ sighting: s.id, file: s.file, ...rt, check: s.check, ...check });
}

const out = { sdk: "js-stellar-sdk", version, corpus, vectors };
writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
const bad = corpus.filter((c) => !c.roundTripIdentical).length;
console.log(`js-stellar-sdk ${version}: ${corpus.length - bad}/${corpus.length} corpus files round-trip; ` +
  vectors.map((v) => `${v.sighting} ${v.check}=${v.result}`).join(" "));
