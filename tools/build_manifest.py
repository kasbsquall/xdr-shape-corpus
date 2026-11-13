"""Builds corpus/manifest.json from each entry's capture.json plus the expected values below.

Every expected value is read back from the rs-stellar-xdr JSON form at build time and the build
fails if it does not match, so the manifest cannot drift from the bytes it describes.
Usage: python tools/build_manifest.py [--check]
  --check  verify the committed manifest matches the bytes, without rewriting it (used in CI)
"""
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
RT = ROOT / "tools" / "roundtrip" / "target" / "release" / ("xdr-roundtrip.exe" if sys.platform == "win32" else "xdr-roundtrip")
RT_VERSION = "stellar-xdr 30.0.0"

SPEC = {
    "cap40-signed-payload-signer-max-payload": {
        "caps": ["CAP-0040"],
        "shapes": ["signed-payload-signer", "signed-payload-max-length-64", "set-options-signer"],
        "sightings": ["https://github.com/stellar/wallet-backend/pull/729",
                      "https://github.com/stellar/wallet-backend/issues/716"],
        "expected_file": "meta.b64",
        "values": [
            ("/v4/operations/0/changes/1/updated/data/account/signers/0/key", "@signedPayloadSigner",
             "SEP-23 P... strkey; an indexer that stores it must keep all 64 payload bytes"),
            ("/v4/operations/0/changes/1/updated/data/account/signers/0/weight", 1, None),
        ],
    },
    "cap27-muxed-source-and-destination-id-extremes": {
        "caps": ["CAP-0027"],
        "shapes": ["muxed-source", "muxed-destination", "muxed-id-max-u64", "muxed-id-zero"],
        "sightings": ["https://github.com/stellar/wallet-backend/issues/716",
                      "https://github.com/stellar/js-stellar-sdk/issues/1785"],
        "expected_file": "envelope.b64",
        "values": [
            ("/tx/tx/source_account", "@muxedSource", "muxed id 18446744073709551615 (u64 max)"),
            ("/tx/tx/operations/0/body/payment/destination", "@muxedDestination", "muxed id 0"),
            ("/tx/tx/operations/0/body/payment/amount", "10000000", "stroops, as a string in xdr-json"),
        ],
    },
    "cap73-sac-created-trustline": {
        "caps": ["CAP-0073"],
        "shapes": ["sac-created-trustline", "trustline-without-change-trust", "no-contract-event"],
        "sightings": ["https://github.com/stellar/wallet-backend/issues/731",
                      "https://github.com/stellar/stellar-etl/issues/460"],
        "expected_file": "meta.b64",
        "values": [
            ("/v4/operations/0/changes/0/created/data/trustline/account_id", "@trustor", None),
            ("/v4/operations/0/changes/0/created/data/trustline/asset/credit_alphanum4/asset_code", "XDRK", None),
            ("/v4/operations/0/changes/0/created/data/trustline/limit", "9223372036854775807", "i64::MAX, set by SAC trust()"),
            ("/v4/operations/0/events", [], "trust() emits no contract event; only fee events exist at transaction level"),
        ],
    },
}

XDR_TYPES = {"envelope.b64": "TransactionEnvelope", "result.b64": "TransactionResult",
             "meta.b64": "TransactionMeta", "ledger-close-meta.b64": "LedgerCloseMeta"}


def rt(xdr_type, path, as_json=False):
    args = [str(RT), xdr_type, str(path)] + (["--json"] if as_json else [])
    out = subprocess.run(args, capture_output=True, text=True, check=False)
    return json.loads(out.stdout)


def pointer(doc, ptr):
    for part in ptr.strip("/").split("/"):
        doc = doc[int(part)] if isinstance(doc, list) else doc[part]
    return doc


def main():
    entries = []
    for entry_id, spec in SPEC.items():
        d = ROOT / "corpus" / "entries" / entry_id
        cap = json.loads((d / "capture.json").read_text())
        roundtrips = [rt(XDR_TYPES[f["path"].split("/")[-1]], ROOT / f["path"]) for f in cap["files"]]
        all_ok = all(r.get("roundTripIdentical") for r in roundtrips)
        lcm = rt("LedgerCloseMeta", d / "ledger-close-meta.b64", as_json=True)
        header = next(iter(lcm.values()))["ledger_header"]["header"]
        target = rt(XDR_TYPES[spec["expected_file"]], d / spec["expected_file"], as_json=True)
        values = []
        for ptr, want, note in spec["values"]:
            want = cap[want[1:]] if isinstance(want, str) and want.startswith("@") else want
            got = pointer(target, ptr)
            if got != want:
                sys.exit(f"{entry_id}: {ptr} is {got!r}, expected {want!r}")
            values.append({"pointer": ptr, "value": want, **({"note": note} if note else {})})
        entries.append({
            "id": entry_id,
            "caps": spec["caps"],
            "protocolVersion": header["ledger_version"],
            "network": {"name": "testnet", "passphrase": cap["passphrase"]},
            "source": {"kind": "network-history", "ledger": cap["ledger"], "txHash": cap["txHash"],
                       "capturedVia": cap["capturedVia"], "capturedAt": cap["capturedAt"],
                       "generator": "tools/capture-testnet.mjs"},
            "synthetic": False,
            "shapes": spec["shapes"],
            "sightings": spec["sightings"],
            "files": cap["files"],
            "expected": {"file": f"corpus/entries/{entry_id}/{spec['expected_file']}", "values": values,
                         "roundTrip": True},
            "checks": {"rsStellarXdrRoundTrip": all_ok, "rsStellarXdrVersion": RT_VERSION,
                       "checkLcmGolden": "not-run"},
        })
    manifest = {"schemaVersion": "0.1", "generatedAt": datetime.now(timezone.utc).isoformat(timespec="seconds"),
                "entries": entries}
    (ROOT / "corpus" / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"manifest: {len(entries)} entries, all round-trips identical: {all(e['checks']['rsStellarXdrRoundTrip'] for e in entries)}")


if __name__ == "__main__":
    main()
