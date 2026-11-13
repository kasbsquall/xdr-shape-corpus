"""Validates corpus/manifest.json against schema/manifest.schema.json (JSON Schema 2020-12)
and checks that every file listed in the manifest exists with the recorded sha256 of its decoded raw bytes."""
import base64
import hashlib
import json
import sys
from pathlib import Path

from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parents[1]
schema = json.loads((ROOT / "schema" / "manifest.schema.json").read_text(encoding="utf-8"))
manifest = json.loads((ROOT / "corpus" / "manifest.json").read_text(encoding="utf-8"))

errors = sorted(Draft202012Validator(schema).iter_errors(manifest), key=lambda e: list(e.path))
for e in errors:
    print(f"schema: {'/'.join(map(str, e.path))}: {e.message}")

for entry in manifest["entries"]:
    for f in entry["files"]:
        digest = hashlib.sha256(base64.b64decode((ROOT / f["path"]).read_bytes().strip(), validate=True)).hexdigest()
        if digest != f["sha256"]:
            errors.append(f)
            print(f"sha256 mismatch: {f['path']}")

if errors:
    sys.exit(1)
print(f"schema and hashes ok: {len(manifest['entries'])} entries")
