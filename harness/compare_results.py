"""Fails if a fresh harness run disagrees with the committed results.json.

Usage: python compare_results.py <committed results.json> <fresh results.json>

Compares outcomes only (per commit and entry, and per sighting row), not the
free-text details, which can differ by platform.
"""
import json
import sys


def project(path):
    with open(path, encoding="utf-8") as f:
        d = json.load(f)
    commits = {
        (c["label"], c["dependencyMode"]): (c["commit"], c["goStellarSdk"], c["outcomes"])
        for c in d["commits"]
    }
    sightings = {
        (s["sighting"], s["entry"], s["dependencyMode"]): (
            s["reproduced"],
            [(r["label"], r["commit"], r["outcome"]) for r in s["runs"]],
        )
        for s in d["sightings"]
    }
    return commits, sightings


def main():
    expected, fresh = project(sys.argv[1]), project(sys.argv[2])
    failed = False
    for name, exp, got in (("commit", expected[0], fresh[0]), ("sighting", expected[1], fresh[1])):
        for key in sorted(set(exp) | set(got), key=str):
            if exp.get(key) != got.get(key):
                failed = True
                print(f"{name} {key}: committed {exp.get(key)} != fresh {got.get(key)}")
    if failed:
        sys.exit(1)
    print(f"{len(expected[0])} commit runs and {len(expected[1])} sighting rows match the committed results")


if __name__ == "__main__":
    main()
