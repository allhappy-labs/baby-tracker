#!/bin/sh
# Renders index.html at iPhone scale (3x) and 1x.
D="$(cd "$(dirname "$0")" && pwd)"
C="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
python3 "$D/build.py"
"$C" --headless=new --disable-gpu --allow-file-access-from-files --hide-scrollbars --force-device-scale-factor=3 --window-size=700,900 --screenshot="$D/preview@3x.png" "file://$D/index.html" 2>/dev/null
"$C" --headless=new --disable-gpu --allow-file-access-from-files --hide-scrollbars --force-device-scale-factor=1 --window-size=700,900 --screenshot="$D/preview.png" "file://$D/index.html" 2>/dev/null
