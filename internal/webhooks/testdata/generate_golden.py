#!/usr/bin/env python3
"""Golden-vector generator for Orvexa webhook provider verifiers (issue #24).

ALL CREDENTIALS IN THIS FILE ARE SYNTHETIC AND CLEARLY FAKE. Hostnames use the
reserved .example TLD; tokens/secrets are obviously-fake hex. Nothing here can
authenticate against any real carrier.

The vectors are produced with Python 3.12's stdlib (hmac/hashlib) — an
implementation INDEPENDENT of the Go verifier under test — so the Go test
suite proves cross-implementation agreement of the captured spec, not
self-consistency:

  Twilio:          X-Twilio-Signature = base64(HMAC-SHA1(authToken,
                       fullURL + concat(sorted "key+value" pairs)))
                   Params are the URL-DECODED x-www-form-urlencoded values,
                   sorted by key (ties: value) in Unicode code-point order
                   (Go's byte-wise UTF-8 sort is provably identical — verified
                   in verify_test.go).
  WhatsApp Cloud   x-hub-signature-256 = "sha256=" + hex(HMAC-SHA256(appSecret,
                   rawBody)) — lowercase hex, exact raw body bytes.
  Africa's Talking: no signature surface exists (see
                   verify_africastalking.go doc); its fixture is an IP
                   allowlist decision matrix, not a MAC.

Regenerate:  python3 testdata/generate_golden.py   (writes the *_golden.json
and africastalking_allowlist.json next to this script; deterministic output).
"""
import base64
import hashlib
import hmac
import json
import pathlib
import urllib.parse

HERE = pathlib.Path(__file__).resolve().parent

# --- synthetic credentials (clearly fake, fixed for reproducibility) --------
TWILIO_TOKEN_PRIMARY = "c0ffee00deadbeefc0ffee00deadbeef"  # 32 chars, fake
TWILIO_TOKEN_ROTATED = "feedface00000000feedface00000001"  # rotation token
TWILIO_URL_BASE = "https://webhook.orvexa.example/v1/webhooks/twilio"
WA_APP_SECRET = "a1b2c3d4e5f60718293a4b5c6d7e8f90"          # 32 hex, fake
WA_URL = "https://webhook.orvexa.example/v1/webhooks/whatsappcloud"


def twilio_signature(auth_token: str, url: str, form_body: str) -> str:
    """Canonical Twilio request-validator algorithm (independent impl)."""
    params = urllib.parse.parse_qsl(form_body, keep_blank_values=True)
    signed = url + "".join(k + v for k, v in sorted(params))
    mac = hmac.new(auth_token.encode("utf-8"), signed.encode("utf-8"), hashlib.sha1)
    return base64.b64encode(mac.digest()).decode("ascii")


def wa_signature(app_secret: str, raw_body: str) -> str:
    mac = hmac.new(app_secret.encode("utf-8"), raw_body.encode("utf-8"), hashlib.sha256)
    return "sha256=" + mac.hexdigest()


def twilio_vectors():
    v = []

    # 1. basic multi-param status callback (E.164 numbers percent-encoded)
    body = ("CallSid=CAf00dface0123456789abcdef01234567"
            "&CallStatus=ringing"
            "&From=%2B15550001111"
            "&To=%2B15550002222")
    v.append({
        "name": "basic-multi-param",
        "auth_token": TWILIO_TOKEN_PRIMARY,
        "url": TWILIO_URL_BASE,
        "form_body": body,
        "expected_signature_header": twilio_signature(TWILIO_TOKEN_PRIMARY, TWILIO_URL_BASE, body),
    })

    # 2. sort-order proof: case-sensitive byte order + UTF-8 multi-byte keys.
    #    Code-point order: "B_param"(0x42) < "a_param"(0x61) < "b_param"(0x62)
    #    < "emoji"(0x65) < "名前"(0x540D...). Also proves values may contain
    #    multi-byte characters.
    body = ("a_param=alpha"
            "&b_param=beta"
            "&B_param=Bravo"
            "&emoji=%F0%9F%93%9E%E2%98%8E"
            "&%E5%90%8D%E5%89%8D=%E5%B1%B1%E7%94%B0")
    v.append({
        "name": "sorted-params-case-and-unicode",
        "auth_token": TWILIO_TOKEN_PRIMARY,
        "url": TWILIO_URL_BASE,
        "form_body": body,
        "expected_signature_header": twilio_signature(TWILIO_TOKEN_PRIMARY, TWILIO_URL_BASE, body),
    })

    # 3. URL-with-query-string: Twilio signs the FULL dialed URL including
    #    the query string (proxy/URL-reconstruction pitfall).
    url = TWILIO_URL_BASE + "?accountSid=AC0123456789abcdef0123456789abcdef&v=2"
    body = "Digits=1234"
    v.append({
        "name": "url-with-query-string",
        "auth_token": TWILIO_TOKEN_PRIMARY,
        "url": url,
        "form_body": body,
        "expected_signature_header": twilio_signature(TWILIO_TOKEN_PRIMARY, url, body),
    })

    # 4. percent/plus decoding pitfall: raw "+" decodes to SPACE, "%2B"
    #    decodes to "+", "%23" decodes to "#" — the signature covers the
    #    DECODED values, so a verifier that signs the raw body fails here.
    body = "SpeechResult=hello+world&AnsweredBy=%2B15551230000&Digits=%23"
    v.append({
        "name": "plus-and-percent-encoding",
        "auth_token": TWILIO_TOKEN_PRIMARY,
        "url": TWILIO_URL_BASE,
        "form_body": body,
        "expected_signature_header": twilio_signature(TWILIO_TOKEN_PRIMARY, TWILIO_URL_BASE, body),
    })

    # 5. signed by the ROTATED (second) token: multi-token acceptance.
    body = "CallStatus=completed&CallSid=CAcafebabedeadbeefcafebabedeadbeef"
    v.append({
        "name": "rotated-second-token",
        "auth_token": TWILIO_TOKEN_ROTATED,
        "url": TWILIO_URL_BASE,
        "form_body": body,
        "expected_signature_header": twilio_signature(TWILIO_TOKEN_ROTATED, TWILIO_URL_BASE, body),
    })

    # 6. duplicate keys: canonical validators sort (key, value) TUPLES, so the
    #    value is the tie-breaker ("Foo=1" before "Foo=2") — order of arrival
    #    in the body must not matter.
    body = "Foo=2&Foo=1&Bar=x"
    v.append({
        "name": "duplicate-keys-value-tiebreak",
        "auth_token": TWILIO_TOKEN_PRIMARY,
        "url": TWILIO_URL_BASE,
        "form_body": body,
        "expected_signature_header": twilio_signature(TWILIO_TOKEN_PRIMARY, TWILIO_URL_BASE, body),
    })
    return v


def wa_vectors():
    v = []

    def add(name, raw):
        v.append({
            "name": name,
            "app_secret": WA_APP_SECRET,
            "raw_body": raw,
            "expected_signature_header": wa_signature(WA_APP_SECRET, raw),
        })

    add("simple-json",
        '{"event":"call.ringing","interaction_id":"9d2b3f4a-1111-4222-8333-c444d555e666","tenant_id":"t1"}')
    # unicode payload — exact UTF-8 bytes are signed
    add("unicode-body",
        '{"event":"message.delivered","detail":"配信済み 📬","tenant_id":"t-multibyte"}')
    # trailing newline: the signature binds the EXACT body bytes, newline included
    add("trailing-newline-body",
        '{"event":"call.completed"}\n')
    add("empty-object", "{}")
    return v


def main():
    twilio = {
        "provider": "twilio",
        "scheme": "X-Twilio-Signature = base64(HMAC-SHA1(authToken, fullURL + concat(sorted key+value)))",
        "provenance": "generated by testdata/generate_golden.py with Python 3.12 stdlib hmac/hashlib (independent of the Go verifier)",
        "credentials": "SYNTHETIC AND CLEARLY FAKE — reserved .example host, fixed dummy tokens",
        "vectors": twilio_vectors(),
    }
    wa = {
        "provider": "whatsappcloud",
        "scheme": 'x-hub-signature-256: "sha256=" + hex(HMAC-SHA256(appSecret, rawBody))',
        "provenance": "generated by testdata/generate_golden.py with Python 3.12 stdlib hmac/hashlib (independent of the Go verifier)",
        "credentials": "SYNTHETIC AND CLEARLY FAKE — reserved .example host, fixed dummy secret",
        "vectors": wa_vectors(),
    }
    at = {
        "provider": "africasTalking",
        "scheme": "NO SIGNATURE SURFACE — documented AT callbacks carry no HMAC/checksum; control = peer-IP allowlist (fail-closed when configured)",
        "provenance": "decision matrix fixture (see verify_africastalking.go doc); CIDRs are synthetic doc examples, not AT's published egress ranges",
        "cases": [
            {"name": "peer-in-allowlist-allow", "allowed_cidrs": ["196.250.209.0/24", "10.0.0.0/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "196.250.209.77, 10.10.0.9", "expect": "allow"},
            {"name": "peer-ipv6-in-allowlist-allow", "allowed_cidrs": ["2001:db8:abcd::/48"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "2001:db8:abcd::7", "expect": "allow"},
            {"name": "peer-ipv4in6-unmapped-allow", "allowed_cidrs": ["10.0.0.0/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "::ffff:10.1.2.3", "expect": "allow"},
            {"name": "peer-outside-allowlist-reject", "allowed_cidrs": ["10.0.0.0/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "203.0.113.7", "expect": "reject"},
            {"name": "forged-xff-not-overwritten-reject", "allowed_cidrs": ["10.0.0.0/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "10.0.0.1, 203.0.113.7", "expect": "reject"},
            {"name": "peer-header-missing-fail-closed", "allowed_cidrs": ["10.0.0.0/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "", "expect": "reject"},
            {"name": "peer-garbage-fail-closed", "allowed_cidrs": ["10.0.0.0/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "not-an-ip", "expect": "reject"},
            {"name": "malformed-cidr-entry-fail-closed", "allowed_cidrs": ["10.0.0.1/8"],
             "peer_ip_header": "X-Forwarded-For", "header_value": "10.0.0.1", "expect": "reject"},
            {"name": "allowlist-unconfigured-warn-allow", "allowed_cidrs": [],
             "peer_ip_header": "X-Forwarded-For", "header_value": "any", "expect": "allow-with-structured-warn"},
        ],
    }

    (HERE / "twilio_golden.json").write_text(json.dumps(twilio, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    (HERE / "whatsappcloud_golden.json").write_text(json.dumps(wa, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    (HERE / "africastalking_allowlist.json").write_text(json.dumps(at, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print("wrote twilio_golden.json, whatsappcloud_golden.json, africastalking_allowlist.json")


if __name__ == "__main__":
    main()
