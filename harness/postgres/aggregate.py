"""Builds harness/postgres/results.json from the per-commit runs in harness/postgres/out/."""
import glob
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
ROLES = {"pre-729": "pre-fix", "fix-729": "fixed"}


def summarize(d):
    before, after = d["before"], d["after"]
    changed = {
        t: [before["tableRows"].get(t), n]
        for t, n in sorted(after["tableRows"].items())
        if before["tableRows"].get(t) != n
    }
    runs = []
    for r in d["runs"]:
        attempts = r["persistAttempts"]
        runs.append({
            "run": r["run"],
            "exitCode": r["exitCode"],
            "stoppedBy": r["stoppedBy"],
            "startedAtLedger": r["startedAtLedger"],
            "ingestedLedgers": r["ingestedLedgers"],
            "persistAttemptsMade": r.get("persistAttemptsMade", 0),
            "persistRetryWarnings": len(attempts),
            "persistError": attempts[0]["error"] if attempts else None,
            "finalError": r.get("finalError"),
            "shutdownRequested": r["shutdownRequested"],
        })
    rb = d.get("readBack")
    return {
        "label": d["label"],
        "role": ROLES.get(d["label"], ""),
        "commit": d["commit"],
        "goStellarSdk": d["goStellarSdk"],
        "dependencyMode": d["goStellarSdk"],
        "outcome": d["outcome"],
        "ingestion": d["ingestion"],
        "rollbackObserved": d["rollbackObserved"],
        "cursorBefore": before["cursors"],
        "cursorAfter": after["cursors"],
        "ledgerRowsAfter": after["ledgerRows"],
        "tableRowsChanged": changed,
        "signerRowsAfter": after["signerRows"],
        "readBack": rb,
        "runs": runs,
        "detail": d["detail"],
    }


def main():
    commits = []
    image = entry = ledger = None
    for path in sorted(glob.glob(os.path.join(HERE, "out", "*.json"))):
        with open(path, encoding="utf-8") as f:
            d = json.load(f)
        image, entry, ledger = d["postgresImage"], d["entry"], d["ledger"]
        commits.append(summarize(d))
    order = {k: i for i, k in enumerate(ROLES)}
    commits.sort(key=lambda c: order.get(c["label"], 99))
    pre = [c for c in commits if c["role"] == "pre-fix"]
    fix = [c for c in commits if c["role"] == "fixed"]
    out = {
        "schemaVersion": "0.1",
        "method": "wallet-backend's own `migrate up` and `ingest` commands (live mode, LedgerBackendType rpc) "
                  "against TimescaleDB, ledger served by harness/mockrpc over getLedgers; read back with "
                  "StateChangeModel.BatchGetByAccountAddress",
        "postgresImage": image,
        "entry": entry,
        "ledger": ledger,
        "outcomeVocabulary": ["pass", "mishandled", "dropped"],
        "ingestionVocabulary": ["halted", "advanced", "advanced-without-row", "unexpected"],
        "commits": commits,
        "sighting": {
            "sighting": "wallet-backend#729",
            "entry": entry,
            "reproduced": bool(pre) and bool(fix) and pre[0]["outcome"] != "pass" and all(c["outcome"] == "pass" for c in fix),
            "haltObserved": bool(pre) and pre[0]["ingestion"] == "halted",
            "runs": [{"label": c["label"], "commit": c["commit"], "outcome": c["outcome"], "ingestion": c["ingestion"]} for c in commits],
        },
    }
    with open(os.path.join(HERE, "results.json"), "w", encoding="utf-8", newline="\n") as f:
        json.dump(out, f, indent=2)
        f.write("\n")
    for c in commits:
        print(f"{c['label']:8} {c['commit'][:8]} {c['outcome']:10} {c['ingestion']}")


if __name__ == "__main__":
    main()
