"""Checks the corpus against stellar-core's `--check-lcm` golden LedgerCloseMeta.

What stellar-core's check is (stellar/stellar-core#5435, merged 2026-09-25 as 1b9d421f):
`stellar-core test '[tx]' --rng-seed 12345 --check-lcm <srcdir>` re-runs core's own unit
tests and compares the LedgerCloseMeta *those tests* produce with golden files checked into
core under `test-lcm-current/<TestFile>/<16-hex>.xdr` (`test-lcm-next/` for vnext builds).
Each golden is a stream of record-marked `LedgerCloseMeta`; each directory's `index.json`
maps file names to "test case|section" names and stamps the protocol version and rng seed
that produced the data. Goldens are made with network passphrase `(V) (;,,;) (V)`.
The check has no input for an external LedgerCloseMeta, so a testnet capture cannot be
fed to it, and a testnet ledger can never be byte-equal to a golden (different network,
ledgers and keys). See docs/software/commands.md and src/test/test.cpp in stellar-core.

What "a corpus entry agrees with the goldens" means here, and what this script checks:

1. Decoder and protocol agreement (fails the run). Every golden `.xdr` under
   `test-lcm-current/` at the pinned stellar-core commit decodes with rs-stellar-xdr 30.0.0
   (the version the corpus is checked with) and re-encodes byte-identical; every
   `index.json` lists exactly the `.xdr` files present and is stamped with the protocol
   version of the corpus entries; and every `tx_processing` result pairs with an envelope
   of its transaction set when hashed with core's test passphrase. Some tests close ledgers
   at older protocols before upgrading; those ledgers are decoded but not compared.
2. Entry self-check (fails the run). Each manifest entry's LedgerCloseMeta decodes and
   re-encodes identically, contains the entry's transaction (paired by hash under the entry's
   network passphrase), and that transaction exhibits every shape the manifest lists for
   which a predicate is defined below.
3. Shape agreement (reported, fails only on request). For every shape, the same predicate is
   run over every golden transaction in a ledger at the entry's protocol version. A shape
   location is the JSON path of the rs-stellar-xdr form with array indices dropped, rooted at
   `envelope/` or `tx_processing/`. Per shape:
   `agrees`      core's goldens exhibit the shape at a location where the entry has it;
   `elsewhere`   core's goldens exhibit the shape, but only at other locations;
   `no-golden`   no golden transaction exhibits the shape at the pinned commit;
   `no-predicate` the shape has no predicate here yet (new shapes start like this).
   `--require-agreement` makes `elsewhere` fail the run.

Usage:
  cargo build --release --locked --manifest-path tools/lcm-goldens/Cargo.toml
  python tools/check_lcm_goldens.py [--core-dir DIR] [--write | --check] [--require-agreement]

--write stores the results in corpus/check-lcm.json (only when the run passes), and
tools/build_manifest.py copies each entry's shape statuses from there into the manifest's
`checks.checkLcmGolden`. --check (used in CI) fails when the committed file differs from a fresh
run, so the recorded statuses cannot drift from the goldens or the corpus.

Without --core-dir the script fetches `test-lcm-current/` from stellar-core at CORE_COMMIT
with a sparse, blob-filtered git fetch into a temporary directory (about 80 MB).
"""
import argparse
import base64
import json
import re
import subprocess
import sys
import tempfile
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
# Last stellar-core commit that touched test-lcm-current/ as of 2026-11-12 (protocol 30 data).
CORE_COMMIT = "36fa0b71c3442875a2dc17d1b9d6020e50d33ff1"
CORE_REPO = "https://github.com/stellar/stellar-core.git"
GOLDEN_TIER = "test-lcm-current"
# Network passphrase of all meta captured by stellar-core tests (docs/software/commands.md).
CORE_TEST_PASSPHRASE = "(V) (;,,;) (V)"
U64_MAX = 2**64 - 1
# Committed results of this check; tools/build_manifest.py copies the per-entry statuses into the manifest.
RESULTS = ROOT / "corpus" / "check-lcm.json"
BIN = ROOT / "tools" / "lcm-goldens" / "target" / "release" / ("lcm-goldens.exe" if sys.platform == "win32" else "lcm-goldens")


# ---------------------------------------------------------------- strkeys

def _b32(s):
    return base64.b32decode(s + "=" * (-len(s) % 8))


def muxed_id(strkey):
    """Muxed id of an M... strkey (version byte, 32-byte key, 8-byte id, checksum)."""
    raw = _b32(strkey)
    return int.from_bytes(raw[33:41], "big") if len(raw) == 43 else None


def signed_payload_len(strkey):
    """Payload length of a P... strkey (version, 32-byte key, u32 length, payload, checksum)."""
    raw = _b32(strkey)
    return int.from_bytes(raw[33:37], "big") if len(raw) >= 39 else None


M_KEY = re.compile(r"^M[A-Z2-7]{68}$")
P_KEY = re.compile(r"^P[A-Z2-7]{60,}$")


# ---------------------------------------------------------------- shape predicates

def walk(value, path):
    yield path, value
    if isinstance(value, dict):
        for k, v in value.items():
            yield from walk(v, path + (k,))
    elif isinstance(value, list):
        for v in value:
            yield from walk(v, path + ("*",))


def nodes(tx):
    """All (path, value) pairs of one transaction: its envelope and its tx_processing element.
    Walked once per transaction and cached on it, since every predicate scans the same nodes."""
    if "_nodes" not in tx:
        tx["_nodes"] = [*walk(tx["envelope"], ("envelope",)), *walk(tx["processing"], ("tx_processing",))]
    return tx["_nodes"]


def strings(tx, pattern, key=None):
    for p, v in nodes(tx):
        if isinstance(v, str) and pattern.match(v) and (key is None or p[-1] == key):
            yield p, v


def op_result_kinds(tx):
    """Operation types in the transaction result (also inside a fee bump's inner result)."""
    return {k for p, v in walk(tx["processing"]["result"], ()) if p[-1:] == ("op_inner",) and isinstance(v, dict)
            for k in v}


def tx_succeeded(tx):
    res = tx["processing"]["result"]["result"]["result"]
    return isinstance(res, dict) and ("tx_success" in res or "tx_fee_bump_inner_success" in res)


def created_trustlines(tx):
    for p, v in nodes(tx):
        if p[-1] == "created" and isinstance(v, dict) and "trustline" in v.get("data", {}) and "operations" in p:
            yield p + ("data", "trustline")


def transfer_events_with_muxed_id(tx):
    for p, v in nodes(tx):
        if p[-1] != "events" or "operations" not in p or not isinstance(v, list):
            continue
        for ev in v:
            body = ev.get("body", {}).get("v0", {}) if isinstance(ev, dict) else {}
            topics = body.get("topics", [])
            data = body.get("data", {})
            if topics and topics[0] == {"symbol": "transfer"} and isinstance(data, dict):
                for item in data.get("map", []) or []:
                    if item.get("key") == {"symbol": "to_muxed_id"}:
                        yield p + ("*",), item["val"].get("u64")


def shape_signed_payload_signer(tx):
    return {p for p, _ in strings(tx, P_KEY)}


def shape_signed_payload_max(tx):
    return {p for p, v in strings(tx, P_KEY) if signed_payload_len(v) == 64}


def shape_set_options_signer(tx):
    return {p for p, v in nodes(tx) if p[-2:] == ("set_options", "signer") and v is not None}


def shape_muxed_source(tx):
    return {p for p, _ in strings(tx, M_KEY, "source_account") if p[0] == "envelope"}


def shape_muxed_destination(tx):
    return {p for p, _ in strings(tx, M_KEY, "destination") if p[0] == "envelope"}


def _muxed_id_locations(tx, wanted):
    locs = {p for p, v in strings(tx, M_KEY) if muxed_id(v) == wanted}
    locs |= {p for p, mid in transfer_events_with_muxed_id(tx) if mid is not None and int(mid) == wanted}
    return locs


def shape_sc_address_muxed(tx):
    return {p for p, _ in strings(tx, M_KEY, "address")}


def shape_transfer_to_muxed(tx):
    locs = set()
    for p, v in nodes(tx):
        if p[-1] == "invoke_contract" and isinstance(v, dict) and v.get("function_name") == "transfer":
            if any(isinstance(a, dict) and M_KEY.match(str(a.get("address", ""))) for a in v.get("args", [])):
                locs.add(p)
    return locs


def shape_event_to_muxed_id(tx):
    return {p for p, _ in transfer_events_with_muxed_id(tx)}


def shape_sac_created_trustline(tx):
    if not tx_succeeded(tx) or "invoke_host_function" not in op_result_kinds(tx):
        return set()
    return set(created_trustlines(tx))


def shape_trustline_without_change_trust(tx):
    if not tx_succeeded(tx) or "change_trust" in op_result_kinds(tx):
        return set()
    return set(created_trustlines(tx))


def shape_no_contract_event(tx):
    """A successful host-function call that creates a trustline and emits no contract event
    (SAC `trust()`); the location is the empty operation event list."""
    if not shape_sac_created_trustline(tx):
        return set()
    return {p for p, v in nodes(tx) if p[-1] == "events" and "operations" in p and v == []}


def account_key_bytes(strkey):
    """The 32 raw key bytes of a G... strkey."""
    return _b32(strkey)[1:33]


def shape_issuer_order_differs(tx):
    """An asset pair (pool parameters) whose two assets share type and code, so the issuer decides
    the order, and ordering the issuers by raw key bytes differs from ordering their G... text."""
    locs = set()
    for p, v in nodes(tx):
        if not (isinstance(v, dict) and isinstance(v.get("asset_a"), dict) and isinstance(v.get("asset_b"), dict)):
            continue
        (kind_a, a), (kind_b, b) = next(iter(v["asset_a"].items())), next(iter(v["asset_b"].items()))
        if kind_a != kind_b or not isinstance(a, dict) or a.get("asset_code") != b.get("asset_code"):
            continue
        by_bytes = account_key_bytes(a["issuer"]) < account_key_bytes(b["issuer"])
        by_text = a["issuer"] < b["issuer"]
        if by_bytes != by_text:
            locs.add(p)
    return locs


def shape_pool_share_trustline(tx):
    """A trustline ledger entry for liquidity pool shares, in operation meta."""
    return {p for p, v in nodes(tx)
            if p[-2:] == ("data", "trustline") and "operations" in p and "pool_share" in v.get("asset", {})}


def shape_liquidity_pool_deposit(tx):
    """A LiquidityPoolDeposit operation in a successful transaction whose result reports a deposit success."""
    if not tx_succeeded(tx) or not any(p[-1] == "liquidity_pool_deposit" and v == "success"
                                       for p, v in nodes(tx) if p[0] == "tx_processing"):
        return set()
    return {p for p, _ in nodes(tx) if p[0] == "envelope" and p[-2:] == ("body", "liquidity_pool_deposit")}


INT_SCVALS = {"u32", "i32", "u64", "i64", "u128", "i128", "u256", "i256"}


def scmaps(tx):
    """(path, [key, ...]) for every ScMap: a list under `map` whose items have `key` and `val`."""
    for p, v in nodes(tx):
        if p[-1:] == ("map",) and isinstance(v, list) and v and all(
                isinstance(i, dict) and set(i) == {"key", "val"} for i in v):
            yield p, [i["key"] for i in v]


def native_text(key):
    """How a native conversion would print an ScMap key: integers and strings/symbols as text."""
    if not isinstance(key, dict) or len(key) != 1:
        return None
    (kind, value), = key.items()
    if kind in INT_SCVALS or kind in ("string", "symbol"):
        return str(value)
    return None


def shape_scmap_key_collision(tx):
    """An ScMap with two distinct keys that print as the same text."""
    locs = set()
    for p, keys in scmaps(tx):
        texts = [native_text(k) for k in keys]
        texts = [t for t in texts if t is not None]
        if len(texts) != len(set(texts)):
            locs.add(p)
    return locs


def shape_scmap_u32_string_symbol(tx):
    """An ScMap with a u32, a string and a symbol key that all print as the same text."""
    locs = set()
    for p, keys in scmaps(tx):
        by_text = defaultdict(set)
        for k in keys:
            if isinstance(k, dict) and len(k) == 1:
                (kind, value), = k.items()
                by_text[str(value)].add(kind)
        if any({"u32", "string", "symbol"} <= kinds for kinds in by_text.values()):
            locs.add(p)
    return locs


def shape_event_map_data(tx):
    """A contract event (operation level) whose data is an ScMap."""
    return {p for p, v in nodes(tx) if p[-3:] == ("body", "v0", "data") and "operations" in p
            and isinstance(v, dict) and "map" in v}


def shape_return_map(tx):
    """A Soroban return value that is an ScMap."""
    return {p for p, v in nodes(tx) if p[-2:] == ("soroban_meta", "return_value") and isinstance(v, dict) and "map" in v}


def shape_nested_authorization(tx):
    """A Soroban authorization entry, in a successful transaction, whose root invocation has at
    least one sub-invocation."""
    if not tx_succeeded(tx):
        return set()
    return {p for p, v in nodes(tx) if p[-2:] == ("auth", "*") and isinstance(v, dict)
            and v.get("root_invocation", {}).get("sub_invocations")}


def shape_auth_sub_invocation(tx):
    """A contract-function call inside an authorization tree, below the root invocation, in a
    successful transaction."""
    if not tx_succeeded(tx):
        return set()
    return {p for p, v in nodes(tx) if p[-2:] == ("sub_invocations", "*") and "auth" in p
            and isinstance(v, dict) and "contract_fn" in v.get("function", {})}


def shape_auth_address_signature(tx):
    """Address credentials (any version of the arm) that carry a signature, in a successful transaction."""
    if not tx_succeeded(tx):
        return set()
    return {p for p, v in nodes(tx) if len(p) >= 2 and p[-2] == "credentials" and p[-1].startswith("address")
            and isinstance(v, dict) and v.get("signature") not in (None, "void")}


def shape_list_len(parent_key, length):
    def pred(tx):
        return {p for p, v in nodes(tx) if p[0] == "envelope" and p[-1] == parent_key
                and isinstance(v, list) and len(v) == length}
    return pred


def shape_manage_data_name_64(tx):
    """A ManageData name of 64 bytes (ASCII names; xdr-json escapes other bytes)."""
    return {p for p, v in nodes(tx) if p[-2:] == ("manage_data", "data_name") and isinstance(v, str)
            and "\\" not in v and len(v.encode()) == 64}


def shape_manage_data_value_64(tx):
    """A ManageData value of 64 bytes (128 hex characters in xdr-json)."""
    return {p for p, v in nodes(tx) if p[-2:] == ("manage_data", "data_value") and isinstance(v, str) and len(v) == 128}


def shape_memo_text_28(tx):
    """A text memo of 28 bytes (ASCII memos; xdr-json escapes other bytes)."""
    return {p for p, v in nodes(tx) if p[-2:] == ("memo", "text") and isinstance(v, str)
            and "\\" not in v and len(v.encode()) == 28}


def shape_manage_buy_offer_update(tx):
    """A ManageBuyOffer whose result reports an existing offer updated."""
    if not tx_succeeded(tx):
        return set()
    return {p for p, v in nodes(tx) if p[0] == "tx_processing" and p[-3:] == ("manage_buy_offer", "success", "offer")
            and isinstance(v, dict) and "updated" in v}


def shape_offer_id_in_envelope(tx):
    """A non-zero offer ID in a ManageSellOffer, ManageBuyOffer or CreatePassiveSellOffer operation."""
    return {p for p, v in nodes(tx) if p[0] == "envelope" and p[-1] == "offer_id" and v is not None and int(v) != 0}


SHAPES = {
    "signed-payload-signer": shape_signed_payload_signer,
    "signed-payload-max-length-64": shape_signed_payload_max,
    "set-options-signer": shape_set_options_signer,
    "muxed-source": shape_muxed_source,
    "muxed-destination": shape_muxed_destination,
    "muxed-id-max-u64": lambda tx: _muxed_id_locations(tx, U64_MAX),
    "muxed-id-zero": lambda tx: _muxed_id_locations(tx, 0),
    "sc-address-muxed-account": shape_sc_address_muxed,
    "sac-transfer-to-muxed": shape_transfer_to_muxed,
    "sep41-event-to-muxed-id": shape_event_to_muxed_id,
    "sac-created-trustline": shape_sac_created_trustline,
    "trustline-without-change-trust": shape_trustline_without_change_trust,
    "no-contract-event": shape_no_contract_event,
    "asset-pair-issuer-order-differs-strkey-vs-bytes": shape_issuer_order_differs,
    "pool-share-trustline": shape_pool_share_trustline,
    "liquidity-pool-deposit": shape_liquidity_pool_deposit,
    "scmap-key-collision": shape_scmap_key_collision,
    "scmap-u32-string-symbol-same-text": shape_scmap_u32_string_symbol,
    "contract-event-map-data": shape_event_map_data,
    "contract-return-map": shape_return_map,
    "nested-authorization": shape_nested_authorization,
    "auth-sub-invocation": shape_auth_sub_invocation,
    "auth-address-credentials-signature": shape_auth_address_signature,
    "operations-at-max-100": shape_list_len("operations", 100),
    "extra-signers-at-max-2": shape_list_len("extra_signers", 2),
    "manage-data-name-max-64": shape_manage_data_name_64,
    "manage-data-value-max-64": shape_manage_data_value_64,
    "memo-text-max-28": shape_memo_text_28,
    "manage-buy-offer-update": shape_manage_buy_offer_update,
    "offer-id-in-envelope": shape_offer_id_in_envelope,
}


# ---------------------------------------------------------------- decoding

def decode(files, passphrase, cwd=None):
    """Runs lcm-goldens over files and yields one parsed record per LedgerCloseMeta."""
    proc = subprocess.Popen([str(BIN), "--network-passphrase", passphrase, *files],
                            cwd=cwd, stdout=subprocess.PIPE, text=True, encoding="utf-8")
    for line in proc.stdout:
        yield json.loads(line)
    proc.wait()


def transactions(record):
    """Pairs each tx_processing element with its envelope by transaction hash."""
    lcm = next(iter(record["value"].values()))
    by_hash = {t["hash"]: t["envelope"] for t in record["txSet"]}
    for tp in lcm["tx_processing"]:
        h = tp["result"]["transaction_hash"]
        yield h, {"envelope": by_hash.get(h), "processing": tp}


def ledger_version(record):
    return next(iter(record["value"].values()))["ledger_header"]["header"]["ledger_version"]


def fetch_goldens(dest):
    def git(*args):
        subprocess.run(["git", *args], cwd=dest, check=True, stdout=subprocess.DEVNULL)
    git("init", "-q")
    git("remote", "add", "origin", CORE_REPO)
    git("sparse-checkout", "set", "--no-cone", f"/{GOLDEN_TIER}/")
    git("fetch", "-q", "--depth", "1", "--filter=blob:none", "origin", CORE_COMMIT)
    git("checkout", "-q", "FETCH_HEAD")


def head_commit(core_dir):
    out = subprocess.run(["git", "rev-parse", "HEAD"], cwd=core_dir, capture_output=True, text=True)
    return out.stdout.strip() if out.returncode == 0 else None


# ---------------------------------------------------------------- checks

def check_goldens(core_dir, protocols, errors):
    """Step 1. Returns {(protocol, shape): {location: (golden file, test name)}} over golden
    transactions in ledgers at a corpus protocol version."""
    tier = core_dir / GOLDEN_TIER
    found = defaultdict(dict)
    counts = defaultdict(int)
    for test_dir in sorted(p for p in tier.iterdir() if p.is_dir()):
        index = json.loads((test_dir / "index.json").read_text(encoding="utf-8"))
        listed = {k for k in index if not k.startswith("!")}
        present = {p.stem for p in test_dir.glob("*.xdr")}
        if listed != present:
            errors.append(f"{test_dir.name}: index.json and .xdr files differ: {sorted(listed ^ present)}")
        if index.get("!cfg protocol version") not in protocols:
            errors.append(f"{test_dir.name}: golden protocol {index.get('!cfg protocol version')}, corpus {sorted(protocols)}")
        files = sorted(f"{h}.xdr" for h in present)
        for rec in decode(files, CORE_TEST_PASSPHRASE, cwd=test_dir):
            where = f"{GOLDEN_TIER}/{test_dir.name}/{rec['file']}"
            counts["records"] += 1
            if rec["decode"] != "ok" or not rec["roundTripIdentical"]:
                errors.append(f"{where} record {rec['record']}: {rec.get('error', 'round trip differs')}")
                continue
            version = ledger_version(rec)
            name = index.get(Path(rec["file"]).stem, "?")
            for h, tx in transactions(rec):
                counts["transactions"] += 1
                if tx["envelope"] is None:
                    errors.append(f"{where}: tx {h} has no envelope under passphrase {CORE_TEST_PASSPHRASE!r}")
                    continue
                if version not in protocols:
                    # Tests that start at an older protocol and upgrade; not compared with the corpus.
                    counts["other-protocol transactions"] += 1
                    continue
                for shape, pred in SHAPES.items():
                    for loc in pred(tx):
                        found[(version, shape)].setdefault(loc, (where, name))
        counts["files"] += len(files)
    print(f"goldens: {counts['files']} files, {counts['records']} LedgerCloseMeta, "
          f"{counts['transactions']} transactions at stellar-core {CORE_COMMIT[:10]}; "
          f"{counts['other-protocol transactions']} transactions in ledgers below protocol "
          f"{min(protocols)} are decoded but not compared")
    return found, dict(counts)


def check_entries(manifest, golden_shapes, errors, require_agreement):
    """Steps 2 and 3. Returns the per-entry report."""
    report = []
    for entry in manifest["entries"]:
        lcm_files = [f["path"] for f in entry["files"] if f["xdrType"] == "LedgerCloseMeta"]
        tx_hash = entry.get("source", {}).get("txHash")
        if len(lcm_files) != 1 or not tx_hash:
            errors.append(f"{entry['id']}: needs exactly one LedgerCloseMeta file and a source.txHash")
            continue
        recs = list(decode(lcm_files, entry["network"]["passphrase"], cwd=ROOT))
        if len(recs) != 1 or recs[0]["decode"] != "ok" or not recs[0]["roundTripIdentical"]:
            errors.append(f"{entry['id']}: {lcm_files[0]} does not decode and re-encode identically")
            continue
        tx = dict(transactions(recs[0])).get(tx_hash)
        if tx is None or tx["envelope"] is None:
            errors.append(f"{entry['id']}: transaction {tx_hash} not found in its LedgerCloseMeta")
            continue
        shapes = []
        for shape in entry["shapes"]:
            pred = SHAPES.get(shape)
            if pred is None:
                shapes.append({"shape": shape, "status": "no-predicate"})
                continue
            mine = pred(tx)
            if not mine:
                errors.append(f"{entry['id']}: shape {shape} not found in its own transaction")
                continue
            theirs = golden_shapes.get((entry["protocolVersion"], shape), {})
            common = sorted(mine & theirs.keys())
            if common:
                status = "agrees"
                example = theirs[common[0]]
            else:
                status = "elsewhere" if theirs else "no-golden"
                example = next(iter(theirs.values()), None)
            if status == "elsewhere" and require_agreement:
                errors.append(f"{entry['id']}: shape {shape} found in goldens only at other locations")
            shapes.append({
                "shape": shape, "status": status,
                "entryLocations": sorted("/".join(p) for p in mine),
                "commonLocations": ["/".join(p) for p in common],
                "goldenLocations": sorted("/".join(p) for p in theirs)[:10],
                "goldenExample": {"file": example[0], "test": example[1]} if example else None,
            })
        report.append({"id": entry["id"], "protocolVersion": entry["protocolVersion"], "shapes": shapes})
    return report


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--core-dir", type=Path, help=f"stellar-core checkout at {CORE_COMMIT} (at least {GOLDEN_TIER}/)")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--write", action="store_true", help=f"write the results to {RESULTS.relative_to(ROOT).as_posix()}")
    mode.add_argument("--check", action="store_true", help=f"fail if {RESULTS.relative_to(ROOT).as_posix()} differs from this run")
    ap.add_argument("--require-agreement", action="store_true", help="fail when a shape is only 'elsewhere'")
    args = ap.parse_args()
    if not BIN.exists():
        sys.exit(f"missing {BIN}; build it with: cargo build --release --locked --manifest-path tools/lcm-goldens/Cargo.toml")

    manifest = json.loads((ROOT / "corpus" / "manifest.json").read_text(encoding="utf-8"))
    protocols = {e["protocolVersion"] for e in manifest["entries"]}
    errors = []
    with tempfile.TemporaryDirectory() as tmp:
        core_dir = args.core_dir
        if core_dir is None:
            core_dir = Path(tmp)
            fetch_goldens(core_dir)
        commit = head_commit(core_dir)
        if commit is not None and commit != CORE_COMMIT:
            errors.append(f"{core_dir} is at {commit}, expected {CORE_COMMIT}")
        golden_shapes, counts = check_goldens(core_dir, protocols, errors)
    report = check_entries(manifest, golden_shapes, errors, args.require_agreement)

    for entry in report:
        for s in entry["shapes"]:
            ex = s.get("goldenExample")
            hint = f"  e.g. {ex['file']} ({ex['test']})" if ex else ""
            print(f"{s['status']:<12} {entry['id']} / {s['shape']}{hint}")
    results = {"stellarCoreCommit": CORE_COMMIT, "goldenTier": GOLDEN_TIER, "decoder": "stellar-xdr 30.0.0",
               "goldens": {"files": counts.get("files", 0), "ledgers": counts.get("records", 0),
                           "transactions": counts.get("transactions", 0),
                           "transactionsAtOtherProtocols": counts.get("other-protocol transactions", 0)},
               "entries": report}
    text = json.dumps(results, indent=2) + "\n"
    if args.write and not errors:
        RESULTS.write_text(text, encoding="utf-8")
        print(f"wrote {RESULTS.relative_to(ROOT).as_posix()}; run tools/build_manifest.py to record it in the manifest")
    if args.check:
        committed = json.loads(RESULTS.read_text(encoding="utf-8")) if RESULTS.exists() else None
        if committed != results:
            errors.append(f"{RESULTS.relative_to(ROOT).as_posix()} does not match this run; "
                          "run tools/check_lcm_goldens.py --write, then tools/build_manifest.py")
    for e in errors:
        print(f"FAIL {e}")
    if errors:
        sys.exit(1)
    print(f"check-lcm goldens ok: {len(report)} entries")


if __name__ == "__main__":
    main()
