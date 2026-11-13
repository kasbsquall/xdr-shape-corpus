"""Builds results.json from the per-commit runs in out/.

Usage: python aggregate.py
"""
import glob
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))

# sighting -> (entries, pre-fix label, fixed labels)
SIGHTINGS = {
    "wallet-backend#731": (["cap73-sac-created-trustline"], "pre-731", ["fix-731"]),
    "wallet-backend#729": (["cap40-signed-payload-signer-max-payload"], "pre-729", ["fix-729"]),
    "wallet-backend#716": (
        ["cap40-signed-payload-signer-max-payload", "cap27-muxed-source-and-destination-id-extremes"],
        "pre-716",
        ["fix-729", "close-716"],
    ),
    "wallet-backend#792": (["cap67-sac-transfer-to-muxed-address-max-id"], "pre-792", ["fix-792"]),
}


def load():
    runs = {}
    for path in sorted(glob.glob(os.path.join(HERE, "out", "*.json"))):
        with open(path, encoding="utf-8") as f:
            d = json.load(f)
        tag = "pinned" if path.endswith("-pinned.json") else path.rsplit("-sdk-", 1)[1][:-5]
        runs[(d["label"], tag)] = d
    return runs


def main():
    runs = load()
    commits = []
    for (label, tag), d in sorted(runs.items()):
        commits.append({
            "label": label,
            "commit": d.get("vcsRevision"),
            "goStellarSdk": d.get("goStellarSdk"),
            "dependencyMode": tag,
            "ledgerSource": d.get("ledgerSource", "file"),
            "outcomes": {e["id"]: e["outcome"] for e in d["entries"]},
        })

    rows = []
    for sighting, (entries, pre, fixed) in SIGHTINGS.items():
        for entry in entries:
            for tag in ("pinned", "v0.8.0"):
                cells = []
                for label in [pre] + fixed:
                    d = runs.get((label, tag))
                    if d is None:
                        continue
                    e = next(x for x in d["entries"] if x["id"] == entry)
                    cells.append({
                        "role": "pre-fix" if label == pre else "fixed",
                        "label": label,
                        "commit": d.get("vcsRevision"),
                        "goStellarSdk": d.get("goStellarSdk"),
                        "outcome": e["outcome"],
                        "stage": e["stage"],
                        "detail": e["detail"],
                    })
                if not cells:
                    continue
                pre_out = [c["outcome"] for c in cells if c["role"] == "pre-fix"]
                fix_out = [c["outcome"] for c in cells if c["role"] == "fixed"]
                reproduced = bool(pre_out) and pre_out[0] != "pass" and all(o == "pass" for o in fix_out)
                rows.append({
                    "sighting": sighting,
                    "entry": entry,
                    "dependencyMode": "pinned go-stellar-sdk" if tag == "pinned" else "go-stellar-sdk bumped to v0.8.0",
                    "runs": cells,
                    "reproduced": reproduced,
                })

    out = {
        "schemaVersion": "0.1",
        "adapter": "xdr-kit/harness/go/xdrkitreplay (wallet-backend internal/indexer + BatchCopy column encoders, no database)",
        "outcomeVocabulary": ["pass", "mishandled", "dropped"],
        "commits": commits,
        "sightings": rows,
    }
    with open(os.path.join(HERE, "results.json"), "w", encoding="utf-8") as f:
        json.dump(out, f, indent=2, ensure_ascii=False)
        f.write("\n")
    for r in rows:
        cells = "  ".join(f'{c["label"]}={c["outcome"]}' for c in r["runs"])
        print(f'{r["sighting"]:20} {r["entry"]:48} {r["dependencyMode"]:32} reproduced={r["reproduced"]}  {cells}')


if __name__ == "__main__":
    main()
