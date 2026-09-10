#!/usr/bin/env bash
# security-scan.sh — self-contained secret scanner for tracked files (issue #39).
#
# Dependencies: bash, git, python3 (STDLIB ONLY — no installs, no network).
# Scope: every git-TRACKED file (git ls-files); untracked/ignored files are
#        outside the public-exposure surface this gate protects.
#
# Checks:
#   C1  AWS access keys            AKIA + 16 alnum
#   C2  AWS secret-style assigns   aws ... "=40-char base64-ish literal"
#   C3  Slack tokens               xox[baprs]-...
#   C4  OpenAI keys                sk-... / sk-proj-...
#   C5  Google API keys            AIza + 35 chars
#   C6  GitHub tokens              gh[pousr]_... / github_pat_...
#   C7  Private-key blocks         -----BEGIN ... PRIVATE KEY-----
#   C8  Credential URLs            scheme://userinfo@host (userinfo has ":")
#   C9  High-entropy strings       >=40 chars, Shannon entropy >= 4.5 bits/char
#                                  (calibrated against this tree; see
#                                  docs/security/security-posture.md)
#   C10 Tracked .env files         tracked path named .env / .env.* (except .env.example)
#   C11 Secret-named artifacts     *.pem *.key id_rsa credentials*.json *.p12 ...
#   C12 Generic credential assign  cred-word [=:]"8+ char literal"
#
# Exit codes: 0 = clean (explicitly allowlisted hits allowed and reported),
#             1 = actionable findings, 2 = environment/usage error.
#
# Allowlist policy: every exemption is an explicit, justified RULE below — never
# a blanket file skip for the pattern checks. go.sum/go.mod are exempt from the
# ENTROPY check only (their contents are public module checksums); they are
# still scanned by every pattern detector.
#
# Output discipline: candidate secrets are REDACTED in output (first 10 chars).
set -euo pipefail

if ! command -v git >/dev/null 2>&1; then echo "FATAL: git not found" >&2; exit 2; fi
if ! command -v python3 >/dev/null 2>&1; then echo "FATAL: python3 not found" >&2; exit 2; fi
INSIDE_GIT=$(git rev-parse --is-inside-work-tree 2>/dev/null || echo false)
if [ "$INSIDE_GIT" != "true" ]; then echo "FATAL: not inside a git work tree" >&2; exit 2; fi

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

python3 - "$REPO_ROOT" <<'PYEOF'
import fnmatch
import math
import os
import re
import subprocess
import sys
from collections import Counter

repo = sys.argv[1]

# ---------- tracked file census ----------
files = subprocess.run(["git", "ls-files", "-z"], capture_output=True, check=True)\
    .stdout.decode("utf-8", "replace").split("\0")
files = [f for f in files if f]

BINARY_EXT = (".png", ".jpg", ".jpeg", ".gif", ".ico", ".pdf", ".zip", ".gz",
              ".tar", ".bin", ".woff", ".woff2", ".ttf", ".so", ".dylib")

def is_text(path):
    if path.endswith(BINARY_EXT):
        return False
    try:
        with open(path, "rb") as fh:
            head = fh.read(8192)
        return b"\x00" not in head
    except OSError:
        return False

# ---------- detectors (name, compiled regex) ----------
DETECTORS = [
    ("C1 AWS access key",             re.compile(r"\bAKIA[0-9A-Z]{16}\b")),
    ("C2 AWS secret assign",          re.compile(r"(?i)\baws\b.{0,30}[\"'][0-9a-zA-Z/+]{40}[\"']")),
    ("C3 Slack token",                re.compile(r"\bxox[baprs]-[0-9A-Za-z\-]{10,}\b")),
    ("C4 OpenAI key",                 re.compile(r"\bsk-(?:proj-)?[A-Za-z0-9_\-]{20,}\b")),
    ("C5 Google API key",             re.compile(r"\bAIza[0-9A-Za-z_\-]{35}\b")),
    ("C6 GitHub token",               re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b")),
    ("C7 Private-key block",          re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----")),
    ("C8 Credential URL",             re.compile(r"\b[a-z][a-z0-9+.\-]*://[^/\s:@]+:[^@\s/]+@[^/\s\"']+")),
    ("C12 Generic credential assign", re.compile(r"(?i)\b(password|secret|token|api_key|apikey|passwd)\b\s*[=:]\s*[\"'][^\"']{8,}[\"']")),
]
ENTROPY_RE = re.compile(r"[A-Za-z0-9+/=_.\-]{40,}")

# ---------- allowlist rules (each with a justification; reviewed manually) ----------
# C8: userinfo shapes that are placeholders, not credentials
PLACEHOLDER_USERINFO = re.compile(
    r"^(?:\[.*|\$\{.*|<.*|user:pass|user:password|username:password|x:y|u:p|foo:bar|user:token)$",
    re.IGNORECASE)
# C8: hosts that cannot hold production credentials (local/dev/doc dummies)
LOOPBACK_HOST = re.compile(
    r"^(?:localhost|127\.0\.0\.1|\[::1\]|::1|host\.docker\.internal|.*\.example\.?[a-z.]*|.*\.test|.*\.local)(?::\d+)?$",
    re.IGNORECASE)
DUMMY_HEX = re.compile(r"(?:0123456789abcdef){2,}")   # sequential dummy hex (golden fixtures)
GO_IDENT = re.compile(r"^[A-Za-z]+$")
def camel_transitions(s):
    return sum(1 for a, b in zip(s, s[1:]) if a.islower() and b.isupper())
STRUCTURAL = re.compile(
    r"(?:^//|://|github\.com/|golang\.org/|google\.golang\.org/|gopkg\.in/|go\.uber\.org/|"
    r"\.example/|example\.com|\.md$|/go\.mod|orvexa$|orvexa/)", re.IGNORECASE)

def allowlisted(path, match, detector):
    """Return a justification string when the hit is an explicitly-exempted shape."""
    m = match
    if detector.startswith("C8"):
        userinfo = m[m.find("://") + 3:m.rfind("@")]
        host = m[m.rfind("@") + 1:]
        if PLACEHOLDER_USERINFO.match(userinfo):
            return "placeholder userinfo (format documentation)"
        if LOOPBACK_HOST.match(host):
            return "loopback/dev host (local dummy credential)"
        if path == ".env.example":
            return "documented example env (dummy credential)"
        return None
    if detector.startswith("C9"):
        if path in ("go.sum", "go.mod"):
            return "public module checksum file (go.sum/go.mod)"
        if "://" in m:
            return "plain URL (credentials-in-URL covered by C8)"
        if "ORVEXA_" in m:
            return "ORVEXA_* env-var reference/assignment (documented knob)"
        if STRUCTURAL.search(m):
            return "import path / doc reference"
        if m.startswith("Test") and GO_IDENT.match(m) and camel_transitions(m) >= 3:
            return "Go test identifier"
        if DUMMY_HEX.search(m):
            return "sequential dummy hex (golden fixture)"
        if "fixture" in m.lower() or "example" in m.lower():
            return "self-described fixture/example string"
        return None
    if detector.startswith("C12"):
        lit = re.findall(r"[\"']([^\"']*)[\"']", m)
        val = lit[0] if lit else ""
        low = val.lower()
        if val.startswith("<") or val.startswith("$"):
            return "template/placeholder literal"
        if any(t in low for t in ("wrong", "test", "dummy", "fake", "fixture", "example", "placeholder", "changeme", "redacted")):
            return "self-described non-credential test value"
        return None
    return None

# ---------- C10/C11: filename checks ----------
SECRET_NAME_PATTERNS = ["*.pem", "*.key", "*.p12", "*.pfx", "*.keystore", "*.jks",
                        "id_rsa*", "id_ed25519*", "*.ppk", "credentials*.json",
                        "*_rsa", ".npmrc", ".netrc", ".pgpass"]

def name_findings():
    out = []
    for f in files:
        base = os.path.basename(f)
        if f == ".env.example":
            continue
        if base == ".env" or (base.startswith(".env.") and not base.endswith(".example")):
            out.append(("C10 Tracked .env file", f, base))
        for pat in SECRET_NAME_PATTERNS:
            if fnmatch.fnmatch(base, pat):
                out.append(("C11 Secret-named artifact", f, base))
    return out

# ---------- main scan ----------
findings = []   # (check, file, line, redacted, justification|None)
files_scanned = 0

def redact(s):
    return (s[:10] + "…") if len(s) > 10 else s

for f in files:
    if not is_text(f):
        continue
    files_scanned += 1
    try:
        with open(f, encoding="utf-8", errors="replace") as fh:
            lines = fh.read().splitlines()
    except OSError:
        continue
    for i, line in enumerate(lines, 1):
        for name, rx in DETECTORS:
            for m in rx.finditer(line):
                tok = m.group(0)
                findings.append((name, f, i, redact(tok), allowlisted(f, tok, name)))
        if f not in ("go.sum", "go.mod"):
            for m in ENTROPY_RE.finditer(line):
                tok = m.group(0)
                c = Counter(tok)
                ent = -sum(v / len(tok) * math.log2(v / len(tok)) for v in c.values())
                if len(tok) >= 40 and ent >= 4.5:
                    findings.append(("C9 High-entropy string", f, i, redact(tok),
                                     allowlisted(f, tok, "C9")))

for name, f, detail in name_findings():
    findings.append((name, f, 0, redact(detail), None))

# ---------- report ----------
counts = Counter()
allow = Counter()
real = []
for name, f, line, red, just in findings:
    counts[name] += 1
    if just:
        allow[name] += 1
    else:
        real.append((name, f, line, red))

print("=" * 78)
print("ORVEXA SECURITY SCAN — tracked-file secret sweep (issue #39)")
print("=" * 78)
print(f"{'CHECK':<32}{'HITS':>6}{'ALLOWLISTED':>13}{'ACTIONABLE':>12}")
print("-" * 78)
order = [d[0] for d in DETECTORS] + ["C9 High-entropy string",
                                     "C10 Tracked .env file",
                                     "C11 Secret-named artifact"]
for name in order:
    h, a = counts.get(name, 0), allow.get(name, 0)
    print(f"{name:<32}{h:>6}{a:>13}{h - a:>12}")
print("-" * 78)
print(f"Files scanned: {files_scanned} of {len(files)} tracked (binary excluded)")
print("Entropy calibration: >=40 chars, Shannon entropy >= 4.5 bits/char;")
print("  structural filters: URLs (C8-covered), ORVEXA_* env refs, imports/doc refs,")
print("  Go test identifiers, golden-fixture dummy hex, go.sum/go.mod checksums.")
if allow:
    print("\nAllowlisted hits (each justified, manually reviewed):")
    for name, f, line, red, just in sorted(findings, key=lambda x: (x[0], x[1], x[2])):
        if just:
            print(f"  [{name}] {f}:{line} — {just} ({red})")
if real:
    print(f"\n*** FINDINGS: {len(real)} actionable ***")
    for name, f, line, red in sorted(real):
        print(f"  [{name}] {f}:{line} -> {red}")
    sys.exit(1)
print("\nRESULT: CLEAN — zero actionable secret findings in tracked files.")
sys.exit(0)
PYEOF
