#!/bin/sh
# Bundle the <morph-bot> element from grokbot-animation into the overlay.
#
#   MORPH_BOT_SRC=~/Developer/grokbot-animation ./scripts/build-face.sh
#
# The TV's browser is Chromium 79, so the bundle is lowered to chrome79. The
# component's sampled-voice dialogue system is stubbed out: grokbot speaks
# with xAI TTS, and the stub saves ~2 MB of pinyin dictionary and samples.
set -e
cd "$(dirname "$0")/.."

SRC=${MORPH_BOT_SRC:-$HOME/Developer/grokbot-animation}/component
OUT=overlay/js/vendor/morph-bot.js
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

cp -R "$SRC/." "$TMP/"
rm "$TMP/package.json" # its "sideEffects" list would drop the polyfills
cat > "$TMP/runtime/dialogue-director.js" <<'EOF'
export const DIALOGUE_VOICES = [];
export const DIALOGUE_ENGLISH_MODES = [];
export function compileDialogue() { throw new Error("dialogue not bundled"); }
export class DialogueDirector {
  play() { return Promise.resolve({ cancelled: true }); }
  pause() {}
  resume() {}
  stop() {}
}
EOF
echo 'export function analyzeSpeechUnits() { return []; }' > "$TMP/runtime/speech-unit-analyzer.js"
# The renderer reads the SVG's width every frame right after changing its
# attributes, forcing a layout each time: a third of the frame on the TV's
# CPU. The width only changes with the size attribute, so read it twice a
# second instead.
perl -pi -e 's/this\.svg\.getBoundingClientRect\(\)\.width/(performance.now() < (this.svg.__wAt || 0) ? this.svg.__w : (this.svg.__wAt = performance.now() + 500, this.svg.__w = this.svg.getBoundingClientRect().width))/g' \
  "$TMP/grok-bot-engine.js" "$TMP/runtime/svg-renderer.js"
grep -q "__wAt" "$TMP/runtime/svg-renderer.js" || { echo "width patch did not apply" >&2; exit 1; }
cat > "$TMP/entry.js" <<'EOF'
// Polyfills for what the component uses beyond Chromium 79: at() (92),
// Object.hasOwn (93) and replaceAll (85). Imports are hoisted, so they live
// in a module that is imported first.
import "./polyfills.js";
import "./morph-bot.js";
EOF
cat > "$TMP/polyfills.js" <<'EOF'
function at(i) {
  i = Math.trunc(i) || 0;
  if (i < 0) i += this.length;
  return i < 0 || i >= this.length ? undefined : this[i];
}
for (const proto of [Array.prototype, String.prototype, Float32Array.prototype, Float64Array.prototype, Uint8Array.prototype]) {
  if (!proto.at) Object.defineProperty(proto, "at", { value: at, writable: true, configurable: true });
}
if (!Object.hasOwn) {
  Object.hasOwn = (obj, key) => Object.prototype.hasOwnProperty.call(obj, key);
}
if (!String.prototype.replaceAll) {
  String.prototype.replaceAll = function (find, repl) {
    return typeof find === "string" ? this.split(find).join(repl) : this.replace(find, repl);
  };
}
EOF

mkdir -p "$(dirname "$OUT")"
npx -y esbuild@0.24.0 "$TMP/entry.js" --bundle --format=iife --target=chrome79 --minify \
  --legal-comments=none --log-level=warning --outfile="$OUT"
ls -la "$OUT"
