#!/bin/sh
# Headless check of the dispatch console's emergency banner + alarm against a
# fake in-page server (no network, no real server). Needs Google Chrome.
#   sh webtest/headless/run.sh
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
WEB="$HERE/../../web"
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cp -R "$WEB" "$TMP/site"
cp "$HERE/harness-pre.js" "$HERE/harness-run.js" "$TMP/site/"
python3 - "$TMP/site" <<'PY'
import re, sys, os
d = sys.argv[1]
s = open(os.path.join(d, 'dispatch.html')).read()
s = s.replace('<head>', '<head>\n<script src="harness-pre.js"></script>', 1)
s = re.sub(r'(<script src="dispatch\.js\?v=\d+"></script>)', r'\1\n<script src="harness-run.js"></script>', s)
open(os.path.join(d, 'harness.html'), 'w').write(s)
PY
FAIL=0
for h in "" "#reload"; do
  OUT=$("$CHROME" --headless=new --disable-gpu --no-first-run --window-size=1600,1000 \
    --user-data-dir="$TMP/prof" --allow-file-access-from-files --virtual-time-budget=60000 \
    --dump-dom "file://$TMP/site/harness.html$h" 2>/dev/null | sed -n '/===RESULT===/,/===END===/p')
  echo "$OUT"
  echo "$OUT" | grep -q "ALL PASSED" || FAIL=1
done
exit $FAIL
