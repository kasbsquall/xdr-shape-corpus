"""Conformance runner for py-stellar-xdr (the stellar-xdr package pinned in requirements.txt), the XDR
library the next major version of py-stellar-base will use.

For every corpus file and sighting vector: decode, re-encode and compare the bytes. For every corpus file,
also compare its SEP-51 JSON with the JSON rs-stellar-xdr produces (tools/roundtrip, built by CI before this
runs) and read that JSON back into the same bytes. For every manifest entry, check its expected values
(`expected` and `expectedLedger`) against this library's JSON. Then run the sighting checks that apply to an
XDR library.
Usage: python conformance/python-xdr/run.py <out.json>
"""
import base64
import json
import subprocess
import sys
from importlib.metadata import version
from pathlib import Path

import stellar_xdr as xdr

KIT = Path(__file__).resolve().parents[2]
RT = KIT / "tools" / "roundtrip" / "target" / "release" / ("xdr-roundtrip.exe" if sys.platform == "win32" else "xdr-roundtrip")
XDR_NAMES = {"ScVal": "SCVal"}  # corpus type names that py-stellar-xdr spells differently
INT64 = (-(2**63), 2**63 - 1)
UINT64 = (0, 2**64 - 1)


def xdr_class(xdr_type):
    return getattr(xdr, XDR_NAMES.get(xdr_type, xdr_type))


def attempt(fn):
    """Returns (value, None), or (None, error message) when fn raises."""
    try:
        return fn(), None
    except Exception as e:  # the check is whether it raises at all
        return None, f"{type(e).__name__}: {e}"[:200]


def rust_json(xdr_type, path):
    out = subprocess.run([str(RT), xdr_type, str(path), "--json"], capture_output=True, text=True, check=True)
    return json.loads(out.stdout)


def pointer(doc, ptr):
    for part in ptr.strip("/").split("/"):
        doc = doc[int(part)] if isinstance(doc, list) else doc[part]
    return doc


def corpus_file(xdr_type, path):
    b64 = path.read_text(encoding="utf-8").strip()
    value, err = attempt(lambda: xdr_class(xdr_type).from_xdr_base64(b64))
    if err:
        return {"decode": "error", "error": err, "roundTripIdentical": False}, None
    mine = json.loads(json.dumps(value.to_json_value()))
    rust = rust_json(xdr_type, path)
    back, err = attempt(lambda: xdr_class(xdr_type).from_json_value(rust).to_xdr_base64())
    return {"decode": "ok", "roundTripIdentical": value.to_xdr_base64() == b64,
            "jsonEqualsRust": mine == rust, "rustJsonReadsBackIdentical": err is None and back == b64,
            **({"jsonReadError": err} if err else {})}, mine


def expected_values(entry, jsons):
    results = []
    for block in ("expected", "expectedLedger"):
        if block not in entry:
            continue
        doc = jsons.get(entry[block]["file"])
        for v in entry[block]["values"]:
            got, err = attempt(lambda: pointer(doc, v["pointer"]))
            results.append({"file": entry[block]["file"], "pointer": v["pointer"], "match": err is None and got == v["value"],
                            **({"got": got} if err is None and got != v["value"] else {}), **({"error": err} if err else {})})
    return results


def in_range_check(encode, inputs, bounds, name):
    """#1785 at the XDR layer: every canonical integer in the XDR type's range encodes as given; every one
    outside it raises. Whether a negative offer ID makes sense is an SDK rule, not an XDR one."""
    lo, hi = bounds
    ints = sorted({n for n in (_int(v) for v in inputs) if n is not None})
    failures = []
    for n in ints:
        got, err = attempt(lambda: encode(n))
        if lo <= n <= hi and (err or got != n):
            failures.append(f"{n} {'raises ' + err if err else f'encodes {got}'}")
        if not lo <= n <= hi and err is None:
            failures.append(f"{n} accepted as {got}")
    inside = [n for n in ints if lo <= n <= hi]
    outside = [n for n in ints if not lo <= n <= hi]
    if failures:
        return {"result": "defect", "detail": "; ".join(failures)[:300]}
    return {"result": "ok", "detail": f"{name} encodes {inside} as given and raises for {outside} (outside the XDR type)"}


def _int(v):
    try:
        n = int(v)
    except ValueError:
        return None
    return n if str(n) == v else None


def account_key(g):
    return base64.b32decode(g)[1:33]


def muxed_id(s):
    i = s["inputs"]

    def encode(n):
        raw = xdr.MuxedAccount.MuxedEd25519(xdr.MuxedAccountMed25519(id=n, ed25519=account_key(i["account"]))).to_xdr_bytes()
        return xdr.MuxedAccount.from_xdr_bytes(raw).med25519.id

    return in_range_check(encode, i["accepted"] + i["rejected"], UINT64, "MuxedAccountMed25519")


def offer_id(s):
    def encode(n):
        op = xdr.ManageSellOfferOp(selling=xdr.Asset.Native(), buying=xdr.Asset.Native(), amount=1,
                                   price=xdr.Price(n=1, d=1), offer_id=n)
        return xdr.ManageSellOfferOp.from_xdr_bytes(op.to_xdr_bytes()).offer_id

    i = s["inputs"]
    return in_range_check(encode, i["accepted"] + i["rejected"], INT64, "ManageSellOfferOp")


NA = {"result": "n/a", "detail": "an XDR library has no API for this; it is SDK behaviour"}
CHECKS = {
    "native-map-conversion-keeps-every-entry": lambda s, value=None: NA,
    "asset-pair-ordered-by-issuer-bytes": lambda s: NA,
    "scmap-keys-sorted-like-host": lambda s: NA,
    "muxed-id-encodes-the-id-given": muxed_id,
    "offer-id-encodes-the-id-given": offer_id,
}


def main():
    manifest = json.loads((KIT / "corpus" / "manifest.json").read_text(encoding="utf-8"))
    corpus, values = [], []
    for e in manifest["entries"]:
        jsons = {}
        for f in e["files"]:
            r, mine = corpus_file(f["xdrType"], KIT / f["path"])
            corpus.append({"entry": e["id"], "path": f["path"], "xdrType": f["xdrType"], **r})
            jsons[f["path"]] = mine
        values += [{"entry": e["id"], **v} for v in expected_values(e, jsons)]

    sightings = json.loads((KIT / "conformance" / "sightings.json").read_text(encoding="utf-8"))["sightings"]
    vectors = []
    for s in sightings:
        if "inputs" in s:
            vectors.append({"sighting": s["id"], "check": s["check"], **CHECKS[s["check"]](s)})
            continue
        b64 = (KIT / "conformance" / s["file"]).read_text(encoding="utf-8").strip()
        value, err = attempt(lambda: xdr_class(s["xdrType"]).from_xdr_base64(b64))
        rt = ({"decode": "error", "error": err, "roundTripIdentical": False} if err
              else {"decode": "ok", "roundTripIdentical": value.to_xdr_base64() == b64})
        vectors.append({"sighting": s["id"], "file": s["file"], **rt, "check": s["check"], **CHECKS[s["check"]](s, value)})

    lib = version("stellar-xdr")
    out = {"sdk": "py-stellar-xdr", "version": lib, "protocolVersion": xdr.PROTOCOL_VERSION,
           "corpus": corpus, "expectedValues": values, "vectors": vectors}
    Path(sys.argv[1]).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    rt_ok = sum(c["roundTripIdentical"] for c in corpus)
    js_ok = sum(c.get("jsonEqualsRust", False) for c in corpus)
    back_ok = sum(c.get("rustJsonReadsBackIdentical", False) for c in corpus)
    ev_ok = sum(v["match"] for v in values)
    print(f"py-stellar-xdr {lib} (protocol {xdr.PROTOCOL_VERSION}): {rt_ok}/{len(corpus)} files round-trip; "
          f"JSON equals rs-stellar-xdr for {js_ok}/{len(corpus)}; rs JSON reads back for {back_ok}/{len(corpus)}; "
          f"{ev_ok}/{len(values)} expected values match; "
          + " ".join(f"{v['sighting']} {v['check']}={v['result']}" for v in vectors))
    if rt_ok != len(corpus) or js_ok != len(corpus) or back_ok != len(corpus) or ev_ok != len(values):
        sys.exit(1)


if __name__ == "__main__":
    main()
