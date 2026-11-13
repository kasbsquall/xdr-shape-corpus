"""Fails if a fresh Postgres harness run disagrees with the committed results.json.

Usage: python compare_results.py <committed results.json> <fresh results.json>

Compares what was observed in the database and in wallet-backend's logs: outcome, whether
ingestion halted or advanced, cursors, row counts, persist attempts, error text, signer
read-back. Leaves out the free-text detail and anything with a timestamp.
"""
import json
import sys


def project(path):
    with open(path, encoding="utf-8") as f:
        d = json.load(f)
    commits = {}
    for c in d["commits"]:
        commits[c["label"]] = {
            "commit": c["commit"],
            "goStellarSdk": c["goStellarSdk"],
            "outcome": c["outcome"],
            "ingestion": c["ingestion"],
            "rollbackObserved": c["rollbackObserved"],
            "cursorBefore": c["cursorBefore"],
            "cursorAfter": c["cursorAfter"],
            "ledgerRowsAfter": c["ledgerRowsAfter"],
            "tableRowsChanged": c["tableRowsChanged"],
            "signerRowsAfter": c["signerRowsAfter"],
            "readBack": c["readBack"],
            "runs": [
                {k: r[k] for k in ("run", "exitCode", "stoppedBy", "startedAtLedger", "ingestedLedgers",
                                   "persistAttemptsMade", "persistRetryWarnings", "persistError", "finalError",
                                   "shutdownRequested")}
                for r in c["runs"]
            ],
        }
    s = d["sighting"]
    sighting = {k: s[k] for k in ("sighting", "entry", "reproduced", "haltObserved", "runs")}
    return {"postgresImage": d["postgresImage"], "ledger": d["ledger"], "commits": commits, "sighting": sighting}


def diff(path, exp, got, out):
    if isinstance(exp, dict) and isinstance(got, dict):
        for k in sorted(set(exp) | set(got), key=str):
            diff(f"{path}.{k}", exp.get(k), got.get(k), out)
    elif exp != got:
        out.append(f"{path}: committed {exp!r} != fresh {got!r}")


def main():
    exp, got = project(sys.argv[1]), project(sys.argv[2])
    problems = []
    diff("results", exp, got, problems)
    if problems:
        print("\n".join(problems))
        sys.exit(1)
    print(f"{len(exp['commits'])} commit runs match the committed results "
          f"(#729 reproduced: {exp['sighting']['reproduced']}, halt observed: {exp['sighting']['haltObserved']})")


if __name__ == "__main__":
    main()
