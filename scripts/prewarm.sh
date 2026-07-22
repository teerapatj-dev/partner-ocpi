#!/usr/bin/env bash
# Wake all 3 partners (Render free sleeps after ~15 min idle; Neon autosuspends
# too). Run this before any test/demo — Evolt's outbound timeout is 30s with no
# retry, so a cold-start push would otherwise fail.
#
# Usage: ./scripts/prewarm.sh https://plg.example https://vct.example https://chx.example
#        ./scripts/prewarm.sh            # defaults to the local compose ports
set -u

urls=("$@")
if [ ${#urls[@]} -eq 0 ]; then
  urls=(http://localhost:9101 http://localhost:9102 http://localhost:9103)
fi

for url in "${urls[@]}"; do
  printf 'warming %s ' "$url"
  for _ in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$url/health" || true)
    if [ "$code" = "200" ]; then
      printf 'ok\n'
      continue 2
    fi
    printf '.'
    sleep 3
  done
  printf ' FAILED\n'
done
