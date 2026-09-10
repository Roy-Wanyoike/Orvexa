#!/usr/bin/env bash
# scripts/sbom.sh — CycloneDX 1.5 SBOM for Orvexa ([O-37], issue #46).
#
# Self-contained by contract: parses go.mod with the python3 STANDARD LIBRARY
# only — no cyclonedx-gomod, no syft, no network access. Every required
# module (direct and // indirect) becomes a component "module@version".
#
# Idempotent by construction: the serialNumber is a UUIDv5 over the exact
# component set and metadata.timestamp is the HEAD commit date, so the same
# tree produces byte-identical output (safe to re-run in CI; the file is
# replaced atomically via a temp file + rename).
#
# Usage: scripts/sbom.sh [dist/orvexa.cdx.json]
#   ORVEXA_SBOM_VERSION overrides the metadata.component version stamp
#   (defaults to git describe, falling back to "dev" — same rule as the
#   Makefile release stamping).
#
# Note: go.mod `replace`/`exclude` directives are deliberately not resolved;
# the repository carries none today. The authoritative resolved graph remains
# go.sum / `go list -m all` — if a replace ever lands, revisit this script.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$root/dist/orvexa.cdx.json}"
gomod="$root/go.mod"

command -v python3 >/dev/null 2>&1 || { echo "sbom.sh: python3 is required" >&2; exit 1; }
[ -f "$gomod" ] || { echo "sbom.sh: go.mod not found at $gomod" >&2; exit 1; }

mkdir -p "$(dirname "$out")"
tmp="$(mktemp "$out.XXXXXX.tmp")"
trap 'rm -f "$tmp"' EXIT

# Deterministic timestamp: HEAD commit date (strict ISO 8601); wall clock
# only as a last-resort fallback outside a git checkout.
timestamp="$(git -C "$root" log -1 --format=%cI 2>/dev/null || true)"
[ -n "$timestamp" ] || timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
version="${ORVEXA_SBOM_VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}"

python3 - "$gomod" "$tmp" "$timestamp" "$version" <<'PY'
import json
import pathlib
import sys
import uuid

gomod_path, out_path, timestamp, version = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

mod_name, deps = None, []
in_require = False
for raw in pathlib.Path(gomod_path).read_text(encoding="utf-8").splitlines():
    line = raw.split("//")[0].strip()  # strip line comments (incl. "// indirect")
    if not line:
        continue
    if line.startswith("module "):
        mod_name = line[len("module "):].strip()
        continue
    if line == "require (":
        in_require = True
        continue
    if in_require:
        if line == ")":
            in_require = False
        else:
            parts = line.split()
            if len(parts) >= 2:
                deps.append((parts[0], parts[1]))
        continue
    if line.startswith("require "):
        parts = line[len("require "):].split()
        if len(parts) >= 2:
            deps.append((parts[0], parts[1]))

if not mod_name:
    sys.exit("sbom: go.mod carries no module directive")

def bom_ref(module: str, ref_version: str) -> str:
    return f"pkg:golang/{module.lower()}@{ref_version}"

# Sorted ⇒ deterministic component order.
components = [
    {
        "type": "library",
        "bom-ref": bom_ref(m, v),
        "name": m,
        "version": v,
        "purl": bom_ref(m, v),  # purl type "golang": pkg:golang/<path>@<version>
    }
    for m, v in sorted(deps)
]

# Deterministic serial: UUIDv5 over the exact component set + module name.
material = "orvexa-sbom:" + mod_name + "|" + ",".join(c["bom-ref"] for c in components)
serial = uuid.uuid5(uuid.NAMESPACE_URL, material)

bom = {
    "bomFormat": "CycloneDX",
    "specVersion": "1.5",
    "serialNumber": f"urn:uuid:{serial}",
    "version": 1,
    "metadata": {
        "timestamp": timestamp,
        "tools": [
            {"vendor": "Orvexa", "name": "scripts/sbom.sh", "version": "1.0"}
        ],
        "component": {
            "bom-ref": f"pkg:golang/{mod_name.lower()}@{version}",
            "type": "application",
            "name": "orvexa",
            "version": version,
            "purl": f"pkg:golang/{mod_name.lower()}@{version}",
        },
    },
    "components": components,
}

pathlib.Path(out_path).write_text(
    json.dumps(bom, indent=2, sort_keys=False) + "\n", encoding="utf-8"
)
print(f"sbom: wrote {out_path} ({len(components)} components)")
PY

# ---- validation gate: the artifact must be valid JSON with required fields ----
python3 - "$tmp" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "rb") as f:
    bom = json.load(f)  # raises on invalid JSON

problems = []
if bom.get("bomFormat") != "CycloneDX":
    problems.append("bomFormat must be 'CycloneDX'")
if bom.get("specVersion") != "1.5":
    problems.append("specVersion must be '1.5'")
serial = bom.get("serialNumber", "")
if not serial.startswith("urn:uuid:") or len(serial) != len("urn:uuid:") + 36:
    problems.append("serialNumber must be a urn:uuid")
if bom.get("version") != 1:
    problems.append("bom version must be the integer 1")
meta = bom.get("metadata", {})
if "timestamp" not in meta:
    problems.append("metadata.timestamp missing")
if meta.get("component", {}).get("name") != "orvexa":
    problems.append("metadata.component.name must be 'orvexa'")
tools = meta.get("tools", [])
if not tools or "name" not in tools[0]:
    problems.append("metadata.tools[0].name missing (documented generator)")
for i, c in enumerate(bom.get("components", [])):
    for field in ("type", "name", "version"):
        if not c.get(field):
            problems.append(f"components[{i}].{field} missing")

if problems:
    for problem in problems:
        print(f"sbom validation: {problem}", file=sys.stderr)
    sys.exit(1)
print(
    f"sbom validation: OK — CycloneDX 1.5, serial {serial}, "
    f"{len(bom['components'])} components, tool {tools[0]['name']}"
)
PY

# Validated → atomic publish (temp file + rename keeps partial output impossible).
mv -f "$tmp" "$out"
trap - EXIT

echo "sbom: $(wc -c < "$out") bytes at $out"
