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
    "cap67-sac-transfer-to-muxed-address-max-id": {
        "caps": ["CAP-0067"],
        "shapes": ["sc-address-muxed-account", "sac-transfer-to-muxed", "sep41-event-to-muxed-id", "muxed-id-max-u64"],
        "sightings": ["https://github.com/stellar/wallet-backend/pull/792"],
        "expected_file": "meta.b64",
        "values": [
            ("/v4/operations/0/events/0/body/v0/topics/0/symbol", "transfer", None),
            ("/v4/operations/0/events/0/body/v0/topics/2/address", "@destinationAccount",
             "the event topic carries the base G account, not the M address"),
            ("/v4/operations/0/events/0/body/v0/data/map/1/key/symbol", "to_muxed_id", None),
            ("/v4/operations/0/events/0/body/v0/data/map/1/val/u64", "@muxedDestinationId",
             "u64 max; an indexer that keeps the muxed ID must not narrow it to i64"),
        ],
    },
    "cap38-pool-share-issuers-ordered-by-key-bytes": {
        "caps": ["CAP-0038"],
        "shapes": ["asset-pair-issuer-order-differs-strkey-vs-bytes", "pool-share-trustline", "liquidity-pool-deposit"],
        "sightings": ["https://github.com/stellar/js-stellar-sdk/pull/1766"],
        "expected_file": "envelope.b64",
        "values": [
            ("/tx/tx/operations/0/body/change_trust/line/pool_share/liquidity_pool_constant_product/asset_a/credit_alphanum4/issuer",
             "@issuerFirstByBytes", "first by key bytes, second by G... text"),
            ("/tx/tx/operations/0/body/change_trust/line/pool_share/liquidity_pool_constant_product/asset_b/credit_alphanum4/issuer",
             "@issuerSecondByBytes", "second by key bytes, first by G... text"),
            ("/tx/tx/operations/0/body/change_trust/line/pool_share/liquidity_pool_constant_product/asset_a/credit_alphanum4/asset_code",
             "@assetCode", "both assets share the code, so the issuer decides the order"),
            ("/tx/tx/operations/1/body/liquidity_pool_deposit/liquidity_pool_id",
             "LDK4ZLIKBLFYKNNZJAZA7QMO4LW5RMBPACYBSHYT2AJXHC7LB6PZYJYW", "pool ID derived from the pair in byte order"),
        ],
    },
    "cap46-scmap-keys-collide-after-native-conversion": {
        "caps": ["CAP-0046"],
        "shapes": ["scmap-key-collision", "scmap-u32-string-symbol-same-text", "contract-event-map-data",
                   "contract-return-map"],
        "sightings": ["https://github.com/stellar/js-stellar-sdk/issues/1789",
                      "https://github.com/StellarCN/py-stellar-base/issues/1250"],
        "expected_file": "meta.b64",
        "values": [
            ("/v4/operations/0/events/0/body/v0/data/map",
             [{"key": {"u32": 1}, "val": {"u32": 10}}, {"key": {"string": "1"}, "val": {"u32": 20}},
              {"key": {"symbol": "1"}, "val": {"u32": 30}}],
             "three distinct keys that all print as 1; a native conversion must keep all three or throw"),
            ("/v4/soroban_meta/return_value/map/0/key/u32", 1, None),
            ("/v4/soroban_meta/return_value/map/1/key/string", "1", None),
            ("/v4/soroban_meta/return_value/map/2/key/symbol", "1", None),
        ],
    },
    "cap46-nested-authorization-contract-calls-sac": {
        "caps": ["CAP-0046"],
        "shapes": ["nested-authorization", "auth-sub-invocation", "auth-address-credentials-signature"],
        "sightings": [],
        "expected_file": "envelope.b64",
        "values": [
            ("/tx/tx/operations/0/body/invoke_host_function/auth/0/credentials/address_v2/address", "@authorizer",
             "address credentials signed by an account other than the transaction source"),
            ("/tx/tx/operations/0/body/invoke_host_function/auth/0/root_invocation/function/contract_fn/function_name",
             "pay", None),
            ("/tx/tx/operations/0/body/invoke_host_function/auth/0/root_invocation/sub_invocations/0/function/contract_fn/contract_address",
             "@sacContractId", "native SAC"),
            ("/tx/tx/operations/0/body/invoke_host_function/auth/0/root_invocation/sub_invocations/0/function/contract_fn/function_name",
             "transfer", "sub-invocation one level below the root"),
            ("/tx/tx/operations/0/body/invoke_host_function/auth/0/root_invocation/sub_invocations/0/sub_invocations",
             [], None),
        ],
    },
    "cap21-vectors-at-max-length": {
        "caps": ["CAP-0021", "CAP-0040"],
        "shapes": ["operations-at-max-100", "extra-signers-at-max-2", "signed-payload-max-length-64",
                   "manage-data-name-max-64", "manage-data-value-max-64", "memo-text-max-28"],
        "sightings": [],
        "expected_file": "envelope.b64",
        "values": [
            ("/tx/tx/operations/99/body/manage_data/data_name", "@lastDataName",
             "operation 100 of 100 (MAX_OPS_PER_TX); 64-character name"),
            ("/tx/tx/operations/99/body/manage_data/data_value",
             "636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2",
             "64-byte DataValue, hex in xdr-json"),
            ("/tx/tx/memo/text", "@memoText", "28 bytes"),
            ("/tx/tx/cond/v2/extra_signers/0", "@extraSigner0", "signed-payload signer, 64-byte payload"),
            ("/tx/tx/cond/v2/extra_signers/1", "@extraSigner1", "second of at most two extra signers"),
        ],
    },
    "cap06-manage-buy-offer-update-by-id": {
        "caps": ["CAP-0006"],
        "shapes": ["manage-buy-offer-update", "offer-id-in-envelope"],
        "sightings": [],
        "expected_file": "envelope.b64",
        "values": [
            ("/tx/tx/operations/0/body/manage_buy_offer/offer_id", "@offerId",
             "ID core assigned when the offer was created; in range, since the network only emits IDs it allocated"),
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
    path = ROOT / "corpus" / "manifest.json"
    if "--check" in sys.argv:
        if json.loads(path.read_text())["entries"] != entries:
            sys.exit("corpus/manifest.json does not match the corpus bytes; run tools/build_manifest.py")
        if not all(e["checks"]["rsStellarXdrRoundTrip"] for e in entries):
            sys.exit("a corpus file does not round-trip byte-identical")
        print(f"manifest check ok: {len(entries)} entries, all round-trips identical")
        return
    path.write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"manifest: {len(entries)} entries, all round-trips identical: {all(e['checks']['rsStellarXdrRoundTrip'] for e in entries)}")


if __name__ == "__main__":
    main()
