#!/usr/bin/env bash
# Move the demo to a new public URL in one step.
#
#   deploy/set-public-url.sh https://something.ngrok-free.dev
#
# The tunnel hostname is not just cosmetic: Evolt stores each partner's versions URL when the
# handshake completes, so a URL change leaves every registration pointing at an address that no
# longer answers. This rewrites PUBLIC_BASE_URL, restarts the stack, clears both sides of every
# registration and re-registers all three partners — the sequence that is easy to half-finish by
# hand and then spend an hour debugging as "Evolt is broken".
set -euo pipefail

URL=${1:-}
case "$URL" in
  https://*) ;;
  *) echo "usage: $0 https://<public-host>" >&2; exit 64 ;;
esac
URL=${URL%/}

cd "$(dirname "$0")/.."
[ -f .env.demo ] || { echo ".env.demo not found — copy .env.demo.example first" >&2; exit 1; }
COMPOSE=(docker compose --env-file .env.demo -f docker-compose.yml -f docker-compose.demo.yml)
DEMO=http://localhost:9100

python3 - "$URL" <<'PY'
import re, sys
url = sys.argv[1]
src = open('.env.demo').read()
line = f'PUBLIC_BASE_URL={url}'
out, n = re.subn(r'^PUBLIC_BASE_URL=.*$', line, src, flags=re.M)
open('.env.demo', 'w').write(out if n else src.rstrip('\n') + '\n' + line + '\n')
PY
echo "PUBLIC_BASE_URL -> $URL"

"${COMPOSE[@]}" up -d
# The partner mocks advertise the new BASE_URL only after their restart is through.
for _ in $(seq 30); do
  curl -sf --max-time 3 "$DEMO/healthz" >/dev/null && break
  sleep 1
done

echo "clearing every registration (both sides)…"
curl -sf --max-time 90 -X POST "$DEMO/api/demo/unregister" >/dev/null

for p in plg vct chx; do
  printf '  handshake %s … ' "$p"
  curl -sf --max-time 120 -X POST "$DEMO/api/demo/fanout/$p/handshake" |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print("OK" if d.get("ok") else "FAILED: "+str(d.get("error")))'
done

echo
echo "done — hand QA: $URL"
