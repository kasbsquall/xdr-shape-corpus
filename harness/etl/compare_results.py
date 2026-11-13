"""Fails when a fresh stellar-etl run gives a different outcome than the committed results.

Compares, per entry, the overall outcome and the verdict per export table. Detail strings and
timestamps are not compared.
Usage: python harness/etl/compare_results.py <expected results.json> <fresh results.json>
"""
import json
import sys


def project(path):
    doc = json.loads(open(path, encoding="utf-8").read())
    entries = {e["id"]: (e["outcome"], e.get("perExport", {})) for e in doc["entries"]}
    return doc["commit"], entries


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    want_commit, want = project(sys.argv[1])
    got_commit, got = project(sys.argv[2])
    errors = []
    if want_commit != got_commit:
        errors.append(f"stellar-etl commit {got_commit}, committed results are for {want_commit}")
    for entry_id in sorted(set(want) | set(got)):
        if want.get(entry_id) != got.get(entry_id):
            errors.append(f"{entry_id}: got {got.get(entry_id)}, committed {want.get(entry_id)}")
    for e in errors:
        print(e)
    if errors:
        sys.exit(1)
    print(f"{len(got)} entries match the committed stellar-etl results")


if __name__ == "__main__":
    main()
