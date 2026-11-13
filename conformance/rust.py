"""Conformance runner for rs-stellar-xdr, through tools/roundtrip (stellar-xdr pinned in its Cargo.toml).

Usage: python conformance/rust.py <out.json>
"""
import json
import subprocess
import sys
from pathlib import Path

KIT = Path(__file__).resolve().parents[1]
RT = KIT / "tools" / "roundtrip" / "target" / "release" / ("xdr-roundtrip.exe" if sys.platform == "win32" else "xdr-roundtrip")
VERSION = "30.0.0"


def round_trip(xdr_type, path):
    out = subprocess.run([str(RT), xdr_type, str(path)], capture_output=True, text=True, check=False)
    try:
        r = json.loads(out.stdout)
    except json.JSONDecodeError:
        return {"decode": "error", "error": (out.stderr or out.stdout).strip()[:200], "roundTripIdentical": False}
    ok = r.get("decode") == "ok"
    return {"decode": "ok" if ok else "error", "roundTripIdentical": bool(r.get("roundTripIdentical")),
            **({} if ok else {"error": str(r.get("decode"))[:200]})}


def main():
    manifest = json.loads((KIT / "corpus" / "manifest.json").read_text(encoding="utf-8"))
    corpus = [{"entry": e["id"], "path": f["path"], "xdrType": f["xdrType"], **round_trip(f["xdrType"], KIT / f["path"])}
              for e in manifest["entries"] for f in e["files"]]
    sightings = json.loads((KIT / "conformance" / "sightings.json").read_text(encoding="utf-8"))["sightings"]
    vectors = []
    for s in sightings:
        v = {"sighting": s["id"], "file": s["file"], "xdrType": s["xdrType"],
             **round_trip(s["xdrType"], KIT / "conformance" / s["file"]), "check": s["check"], "result": "n/a"}
        if s["check"] == "native-map-conversion-keeps-every-entry":
            v["detail"] = "no native map conversion in stellar-xdr; ScMap is a vector of entries"
        vectors.append(v)
    Path(sys.argv[1]).write_text(json.dumps({"sdk": "rs-stellar-xdr", "version": VERSION, "corpus": corpus,
                                             "vectors": vectors}, indent=2) + "\n", encoding="utf-8")
    ok = sum(c["roundTripIdentical"] for c in corpus)
    print(f"rs-stellar-xdr {VERSION}: {ok}/{len(corpus)} corpus files round-trip")


if __name__ == "__main__":
    main()
