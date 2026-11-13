"""Conformance runner for py-stellar-base (the stellar-sdk package pinned in requirements.txt).

Decodes every corpus file and every sighting vector with stellar_sdk.xdr, re-encodes it and compares
the bytes, then runs the behaviour check of each sighting.
Usage: python conformance/python/run.py <out.json>
"""
import json
import sys
from importlib.metadata import version
from pathlib import Path

from stellar_sdk import Asset, LiquidityPoolAsset, MuxedAccount, scval, xdr
from stellar_sdk.operation import ManageSellOffer

KIT = Path(__file__).resolve().parents[2]
XDR_NAMES = {"ScVal": "SCVal"}  # corpus type names that py-stellar-base spells differently


def xdr_class(xdr_type):
    return getattr(xdr, XDR_NAMES.get(xdr_type, xdr_type))


def round_trip(xdr_type, b64):
    try:
        again = xdr_class(xdr_type).from_xdr(b64).to_xdr()
    except Exception as e:  # any decode failure is a result, not a crash
        return {"decode": "error", "error": f"{type(e).__name__}: {e}"[:200], "roundTripIdentical": False}
    return {"decode": "ok", "roundTripIdentical": again == b64}


def attempt(fn):
    """Returns (value, None), or (None, error message) when fn raises."""
    try:
        return fn(), None
    except Exception as e:  # the check is whether it raises at all
        return None, f"{type(e).__name__}: {e}"[:100]


def verdict(failures, ok_detail):
    if failures:
        return {"result": "defect", "detail": "; ".join(failures)[:300]}
    return {"result": "ok", "detail": ok_detail}


def canonical_ints(values):
    """The inputs that are canonical decimal integers; Python takes IDs as int, not text."""
    out = []
    for v in values:
        try:
            n = int(v)
        except ValueError:
            continue
        if str(n) == v:
            out.append(n)
    return out


def native_map(s, value):
    """js-stellar-sdk#1789: converting a map keeps every entry or raises."""
    native, err = attempt(lambda: scval.to_native(value))
    if err:
        return {"result": "ok", "detail": f"raises: {err}"}
    kept = len(native)
    return {"result": "ok" if kept == s["mapEntries"] else "defect",
            "detail": f"{s['mapEntries']} entries in XDR, {kept} after scval.to_native"}


def asset_pair_order(s):
    """js-stellar-sdk#1766: assets with the same code order by issuer key bytes."""
    i = s["inputs"]
    first, second = Asset(i["code"], i["issuerFirstByBytes"]), Asset(i["code"], i["issuerSecondByBytes"])
    failures = []
    _, err = attempt(lambda: LiquidityPoolAsset(first, second))
    if err:
        failures.append(f"LiquidityPoolAsset(first, second) raises {err}")
    if attempt(lambda: LiquidityPoolAsset(second, first))[1] is None:
        failures.append("LiquidityPoolAsset(second, first) accepted")
    return verdict(failures, "LiquidityPoolAsset accepts (first, second) and raises for (second, first)")


def sc_val(key):
    (kind, v), = key.items()
    return {"u32": scval.to_uint32, "i32": scval.to_int32, "string": scval.to_string, "symbol": scval.to_symbol,
            "bytes": lambda h: scval.to_bytes(bytes.fromhex(h))}[kind](v)


def scmap_key_order(s):
    """js-stellar-sdk#1766: the sorted-map constructor puts keys in the order the Soroban host requires."""
    keys, want = s["inputs"]["keys"], s["inputs"]["hostOrder"]
    got = [e.key for e in scval.to_map({sc_val(k): scval.to_void() for k in keys}).map.sc_map]
    for n, (g, w) in enumerate(zip(got, want), 1):
        if g.to_xdr() != sc_val(w).to_xdr():
            at = next(k for k in keys if sc_val(k).to_xdr() == g.to_xdr())
            return {"result": "defect", "detail": f"scval.to_map puts {at} at position {n}; host order has {w}"}
    return {"result": "ok", "detail": f"scval.to_map puts all {len(want)} keys in host order"}


def id_check(encode, accepted, rejected, name):
    """Shared by the #1785 checks: encode(n) returns the ID that reaches the XDR, or raises."""
    failures = []
    for n in accepted:
        got, err = attempt(lambda: encode(n))
        if err or got != n:
            failures.append(f"{n} {'raises ' + err if err else f'encodes {got}'}")
    for n in rejected:
        got, err = attempt(lambda: encode(n))
        if err is None:
            failures.append(f"{n} accepted as {got}")
    return verdict(failures, f"{name} keeps {len(accepted)} IDs and raises for {len(rejected)}")


def muxed_id(s):
    """js-stellar-sdk#1785: a muxed account encodes the ID it was given, or raises."""
    i = s["inputs"]

    def encode(n):
        raw = MuxedAccount(i["account"], n).to_xdr_object().to_xdr_bytes()
        return xdr.MuxedAccount.from_xdr_bytes(raw).med25519.id.uint64

    return id_check(encode, canonical_ints(i["accepted"]), canonical_ints(i["rejected"]), "MuxedAccount")


def offer_id(s):
    """js-stellar-sdk#1785: ManageSellOffer encodes the offer ID it was given, or raises."""
    i = s["inputs"]
    buying = Asset("USD", i["issuer"])

    def encode(n):
        raw = ManageSellOffer(Asset.native(), buying, "1", "1", offer_id=n).to_xdr_object().to_xdr_bytes()
        return xdr.Operation.from_xdr_bytes(raw).body.manage_sell_offer_op.offer_id.int64

    return id_check(encode, canonical_ints(i["accepted"]), canonical_ints(i["rejected"]), "ManageSellOffer")


CHECKS = {
    "native-map-conversion-keeps-every-entry": native_map,
    "asset-pair-ordered-by-issuer-bytes": asset_pair_order,
    "scmap-keys-sorted-like-host": scmap_key_order,
    "muxed-id-encodes-the-id-given": muxed_id,
    "offer-id-encodes-the-id-given": offer_id,
}


def main():
    manifest = json.loads((KIT / "corpus" / "manifest.json").read_text(encoding="utf-8"))
    corpus = []
    for e in manifest["entries"]:
        for f in e["files"]:
            b64 = (KIT / f["path"]).read_text(encoding="utf-8").strip()
            corpus.append({"entry": e["id"], "path": f["path"], "xdrType": f["xdrType"], **round_trip(f["xdrType"], b64)})

    sightings = json.loads((KIT / "conformance" / "sightings.json").read_text(encoding="utf-8"))["sightings"]
    vectors = []
    for s in sightings:
        if "inputs" in s:
            vectors.append({"sighting": s["id"], "check": s["check"], **CHECKS[s["check"]](s)})
            continue
        b64 = (KIT / "conformance" / s["file"]).read_text(encoding="utf-8").strip()
        rt = round_trip(s["xdrType"], b64)
        check = CHECKS[s["check"]](s, xdr_class(s["xdrType"]).from_xdr(b64)) if rt["decode"] == "ok" else {"result": "n/a"}
        vectors.append({"sighting": s["id"], "file": s["file"], **rt, "check": s["check"], **check})

    sdk_version = version("stellar-sdk")
    out = {"sdk": "py-stellar-base", "version": sdk_version, "corpus": corpus, "vectors": vectors}
    Path(sys.argv[1]).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    ok = sum(c["roundTripIdentical"] for c in corpus)
    print(f"py-stellar-base {sdk_version}: {ok}/{len(corpus)} corpus files round-trip; "
          + " ".join(f"{v['sighting']} {v['check']}={v['result']}" for v in vectors))


if __name__ == "__main__":
    main()
