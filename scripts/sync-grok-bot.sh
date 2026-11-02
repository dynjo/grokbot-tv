#!/bin/sh
# Copy the Grok Bot app's sign-in from this Mac to the TV, so grokbot can
# pass requests to your Grok Bot agent (grok_bot in config.json).
#
#   TV=root@192.168.1.50 ./scripts/sync-grok-bot.sh
#
# The sign-in is read from the app's encrypted store with its Keychain key
# (macOS may ask to allow access to "Grok Bot Safe Storage"). It lasts a few
# weeks; run this again when the TV says it has expired. The TV never
# renews it, so the app stays signed in.
set -e
: "${TV:?set TV, e.g. TV=root@192.168.1.50}"
DEST=/home/root/.config/grokbot/grok-bot-auth.json

node --input-type=module -e '
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";

const dir = homedir() + "/Library/Application Support/Grok Bot";
const secrets = JSON.parse(readFileSync(dir + "/sand-secrets.json", "utf8"));
const accounts = JSON.parse(secrets["cursor-accounts"]);
const encrypted = accounts.accounts?.[accounts.active]?.["cursor-access-token"];
if (!encrypted) throw new Error("not signed in to the Grok Bot app");

// Chromium Safe Storage on macOS: "v10" + AES-128-CBC, key from the Keychain password.
const password = execFileSync("/usr/bin/security", ["find-generic-password", "-w", "-s", "Grok Bot Safe Storage"], { encoding: "utf8" }).trimEnd();
const key = crypto.pbkdf2Sync(password, "saltysalt", 1003, 16, "sha1");
const buf = Buffer.from(encrypted, "base64");
const decipher = crypto.createDecipheriv("aes-128-cbc", key, Buffer.alloc(16, 32));
const token = Buffer.concat([decipher.update(buf.subarray(3)), decipher.final()]).toString("utf8").trim();

const version = execFileSync("/usr/bin/plutil", ["-extract", "CFBundleShortVersionString", "raw", "/Applications/Grok Bot.app/Contents/Info.plist"], { encoding: "utf8" }).trim();
const exp = JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString() || "{}").exp;
console.error("Grok Bot " + version + ", sign-in valid until " + (exp ? new Date(exp * 1000).toLocaleString() : "unknown"));
process.stdout.write(JSON.stringify({ access_token: token, app_version: version }) + "\n");
' | ssh "$TV" "umask 077; mkdir -p \$(dirname $DEST) && cat > $DEST.tmp && mv $DEST.tmp $DEST && echo copied to $DEST"
