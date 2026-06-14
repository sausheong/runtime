#!/usr/bin/env bash
# Tests deploy/compose/init.sh into a temp HOME so it never touches the real .env.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cp "$HERE/init.sh" "$TMP/init.sh"
chmod +x "$TMP/init.sh"

# First run writes a .env.
"$TMP/init.sh" >/dev/null
ENV="$TMP/.env"
[[ -f "$ENV" ]] || { echo "FAIL: .env not created"; exit 1; }

# Bootstrap key: 64 hex chars.
boot="$(grep '^RUNTIME_ADMIN_BOOTSTRAP=' "$ENV" | cut -d= -f2)"
[[ "$boot" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: bad bootstrap key: $boot"; exit 1; }

# KEYS is id:base64; PRIMARY equals that id.
keys="$(grep '^RUNTIME_SECRETS_KEYS=' "$ENV" | cut -d= -f2-)"
primary="$(grep '^RUNTIME_SECRETS_PRIMARY=' "$ENV" | cut -d= -f2)"
key_id="${keys%%:*}"
key_b64="${keys#*:}"
[[ "$key_id" == "$primary" ]] || { echo "FAIL: PRIMARY($primary) != key id($key_id)"; exit 1; }
# base64 of 32 bytes decodes to exactly 32 bytes.
n="$(printf '%s' "$key_b64" | base64 -d 2>/dev/null | wc -c | tr -d ' ')"
[[ "$n" == "32" ]] || { echo "FAIL: AES key not 32 bytes (got $n)"; exit 1; }

# Second run without --force refuses and leaves .env byte-identical.
before="$(cat "$ENV")"
if "$TMP/init.sh" >/dev/null 2>&1; then echo "FAIL: second run should refuse"; exit 1; fi
[[ "$(cat "$ENV")" == "$before" ]] || { echo "FAIL: refused run modified .env"; exit 1; }

# --force regenerates (new bootstrap value).
"$TMP/init.sh" --force >/dev/null
boot2="$(grep '^RUNTIME_ADMIN_BOOTSTRAP=' "$ENV" | cut -d= -f2)"
[[ "$boot2" != "$boot" ]] || { echo "FAIL: --force did not regenerate"; exit 1; }

echo "PASS: init.sh"
