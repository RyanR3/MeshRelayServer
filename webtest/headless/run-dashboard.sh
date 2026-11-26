#!/bin/sh
# Headless check of the dashboard (index.html + app.js) as a super admin:
# every page opens, and the Settings "Move users" export/preview/import flow,
# against a fake in-page server (no network, no real server). Needs Google Chrome.
#   sh webtest/headless/run-dashboard.sh
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
WEB="$HERE/../../web"
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cp -R "$WEB" "$TMP/site"
cp "$HERE/dashboard-pre.js" "$HERE/dashboard-run.js" "$TMP/site/"
python3 - "$TMP/site" <<'PY'
import re, sys, os
d = sys.argv[1]
s = open(os.path.join(d, 'index.html')).read()
s = re.sub(r'<script src="https://[^"]+"></script>\n?', '', s)   # CDN libraries: stubbed
s = s.replace('<head>', '<head>\n<script src="dashboard-pre.js"></script>', 1)
s = re.sub(r'(<script src="app\.js\?v=\d+"></script>)', r'\1\n<script src="dashboard-run.js"></script>', s)
open(os.path.join(d, 'harness.html'), 'w').write(s)
PY
OUT=$("$CHROME" --headless=new --disable-gpu --no-first-run --window-size=1400,1000 \
  --user-data-dir="$TMP/prof" --allow-file-access-from-files --virtual-time-budget=30000 \
  --dump-dom "file://$TMP/site/harness.html" 2>/dev/null | sed -n '/===RESULT===/,/===END===/p')
echo "$OUT"
echo "$OUT" | grep -q "ALL PASSED"
