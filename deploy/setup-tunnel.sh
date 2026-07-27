#!/usr/bin/env bash
# Put the demo behind a stable public hostname via a named Cloudflare tunnel, and keep it there
# across reboots. Run once per machine:
#
#   cloudflared tunnel login          # you pick the zone in the browser; writes ~/.cloudflared/cert.pem
#   deploy/setup-tunnel.sh ocpi-demo.example.com
#
# Everything below is idempotent — re-running after a change is safe. It never touches .env.demo;
# the last step prints the PUBLIC_BASE_URL line to set, because changing it means re-registering
# with Evolt (the counterparty stores the versions URL at handshake time).
set -euo pipefail

HOST=${1:-}
if [ -z "$HOST" ]; then
  echo "usage: $0 <public-hostname>   e.g. $0 ocpi-demo.example.com" >&2
  exit 64
fi
NAME=${TUNNEL_NAME:-ocpi-demo}
LOCAL=${LOCAL_TARGET:-http://localhost:9100}
CFDIR="$HOME/.cloudflared"
PLIST="$HOME/Library/LaunchAgents/dev.evolt.ocpi-demo.tunnel.plist"
CLOUDFLARED=$(command -v cloudflared || true)

[ -n "$CLOUDFLARED" ] || { echo "cloudflared not installed: brew install cloudflared" >&2; exit 1; }
[ -f "$CFDIR/cert.pem" ] || { echo "not logged in yet — run: cloudflared tunnel login" >&2; exit 1; }

# 1. tunnel (reuse the existing one so the hostname keeps working across re-runs)
if ! "$CLOUDFLARED" tunnel list --output json | grep -q "\"name\":\"$NAME\""; then
  "$CLOUDFLARED" tunnel create "$NAME"
fi
UUID=$("$CLOUDFLARED" tunnel list --output json | python3 -c "
import json,sys
print(next(t['id'] for t in json.load(sys.stdin) if t['name']=='$NAME'))")

# 2. config — one hostname in, everything else refused rather than silently served
cat > "$CFDIR/config.yml" <<YAML
tunnel: $UUID
credentials-file: $CFDIR/$UUID.json

ingress:
  - hostname: $HOST
    service: $LOCAL
  - service: http_status:404
YAML

# 3. DNS: CNAME <host> -> <uuid>.cfargotunnel.com (no-op when it already points there)
"$CLOUDFLARED" tunnel route dns "$NAME" "$HOST" || true

# 4. run it at login. A LaunchAgent (not `cloudflared service install`) keeps this in the user
#    session: no sudo, and it starts alongside Docker Desktop rather than before it.
cat > "$PLIST" <<PLISTXML
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.evolt.ocpi-demo.tunnel</string>
  <key>ProgramArguments</key>
  <array>
    <string>$CLOUDFLARED</string>
    <string>--no-autoupdate</string>
    <string>tunnel</string>
    <string>run</string>
    <string>$NAME</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/ocpi-demo-tunnel.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/ocpi-demo-tunnel.log</string>
</dict>
</plist>
PLISTXML

launchctl unload "$PLIST" 2>/dev/null || true
launchctl load "$PLIST"

echo
echo "tunnel '$NAME' ($UUID) -> $LOCAL, published at https://$HOST"
echo "next:"
echo "  1) set in .env.demo:  PUBLIC_BASE_URL=https://$HOST"
echo "  2) docker compose --env-file .env.demo -f docker-compose.yml -f docker-compose.demo.yml up -d"
echo "  3) re-register every partner (the URL Evolt stored has changed): the demo's"
echo "     'ยกเลิกทะเบียน + ล้าง DB ทุก partner' button, then Handshake for each"
echo "  logs: ~/Library/Logs/ocpi-demo-tunnel.log"
