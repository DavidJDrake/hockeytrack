#!/usr/bin/env bash
# Render site/assets/og/home.png from docs/og/home.html with the countdown
# computed from the live schedule document. Needs google-chrome and python3.
set -euo pipefail
cd "$(dirname "$0")/../.."

CHROME=${CHROME:-google-chrome}
PORT=${PORT:-8479}
SCHEDULE_URL=${SCHEDULE_URL:-https://hockeytrack.davidjdrake.com/data/schedule.json}

query=$(curl -fsS "$SCHEDULE_URL" | python3 -c '
import json, sys, urllib.parse
from datetime import datetime, timezone, timedelta
from zoneinfo import ZoneInfo
doc = json.load(sys.stdin)
now = datetime.now(timezone.utc)
nxt = next((g for g in doc["games"] if datetime.fromisoformat(g["start"].replace("Z", "+00:00")) > now), None)
if not nxt:
    sys.exit("no upcoming game in schedule")
start = datetime.fromisoformat(nxt["start"].replace("Z", "+00:00"))
left = int((start - now).total_seconds())
d, rem = divmod(left, 86400); h, rem = divmod(rem, 3600); m, s = divmod(rem, 60)
et = start.astimezone(ZoneInfo("America/New_York"))
when = et.strftime("%a, %b ") + str(et.day) + " · " + et.strftime("%I:%M %p").lstrip("0") + " ET"
print(urllib.parse.urlencode({"d": d, "h": f"{h:02d}", "m": f"{m:02d}", "s": f"{s:02d}", "match": nxt["away"] + " @ " + nxt["home"], "when": when}))
')

# Serve the site directory so /assets/... resolves exactly as it does in
# production; the template is copied in for the duration of the render.
cp docs/og/home.html site/.og-home.html
cp docs/og/ice.jpg site/.og-ice.jpg
(cd site && python3 -m http.server "$PORT" --bind 127.0.0.1 >/dev/null 2>&1) &
server=$!
trap 'kill $server 2>/dev/null || true; rm -f site/.og-home.html site/.og-ice.jpg' EXIT
sleep 1

out=$(mktemp -d)
"$CHROME" --headless=new --no-sandbox --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
  --window-size=1200,630 --screenshot="$out/home.png" "http://127.0.0.1:$PORT/.og-home.html?$query" >/dev/null 2>&1
mv "$out/home.png" site/assets/og/home.png
echo "rendered site/assets/og/home.png ($query)"
