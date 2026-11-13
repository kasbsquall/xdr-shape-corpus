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
const version = JSON.parse(readFileSync(join(sdkRoot, "node_modules/@stellar/stellar-sdk/package.json"), "utf8")).version;

function roundTrip(xdrType, b64) {
  try {
    const value = sdk.xdr[xdrType].fromXDR(b64, "base64");
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

// Behaviour checks, one per sighting. Each returns "defect" (the sighting's bug is present),
// "ok" (behaves as the fix requires) or "n/a".
const checks = {
  // js-stellar-sdk#1789: converting a map must keep every entry or throw.
  "native-map-conversion-keeps-every-entry": (value, vector) => {
    let native;
    try {
      native = sdk.scValToNative(value);
    } catch (e) {
      return { result: "ok", detail: `throws: ${String(e.message).slice(0, 120)}` };
    }
    const kept = Object.keys(native).length;
    return {
      result: kept === vector.mapEntries ? "ok" : "defect",
      detail: `${vector.mapEntries} entries in XDR, ${kept} after scValToNative`,
    };
  },
};

const sightings = JSON.parse(readFileSync(join(HERE, "..", "sightings.json"), "utf8")).sightings;
const vectors = [];
for (const s of sightings) {
  const b64 = readFileSync(join(HERE, "..", s.file), "utf8").trim();
  const rt = roundTrip(s.xdrType, b64);
  let check = { result: "n/a" };
  if (rt.decode === "ok" && checks[s.check]) {
    check = checks[s.check](sdk.xdr[s.xdrType].fromXDR(b64, "base64"), s);
  }
  vectors.push({ sighting: s.id, file: s.file, ...rt, check: s.check, ...check });
}

const out = { sdk: "js-stellar-sdk", version, corpus, vectors };
writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
const bad = corpus.filter((c) => !c.roundTripIdentical).length;
console.log(`js-stellar-sdk ${version}: ${corpus.length - bad}/${corpus.length} corpus files round-trip; ` +
  vectors.map((v) => `${v.sighting}=${v.result}`).join(" "));
