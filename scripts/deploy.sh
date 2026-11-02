#!/bin/sh
# Install grokbot on a rooted TV over SSH and (re)start it.
#
#   TV=root@192.168.1.50 ./scripts/deploy.sh
#
# Copies grokbotd and run.sh to /home/root/grokbot, installs the
# overlay app, adds a Homebrew Channel boot hook, and writes a config from
# config.example.json on first install (XAI_API_KEY fills in the key).
set -e
cd "$(dirname "$0")/.."

: "${TV:?set TV, e.g. TV=root@192.168.1.50}"
DEST=/home/root/grokbot
CONF_DIR=/home/root/.config/grokbot
HOOK=/var/lib/webosbrew/init.d/50-grokbot

# One SSH connection for the whole deploy, so a password is asked once.
CM=/tmp/grokbot-deploy-$$
SSH="ssh -o ControlMaster=auto -o ControlPath=$CM -o ControlPersist=60"
trap '$SSH -O exit "$TV" 2>/dev/null; true' EXIT

[ -f build/grokbotd ] && ls build/*.ipk >/dev/null 2>&1 || ./scripts/build.sh
IPK=$(ls build/*.ipk | head -1)

echo "stopping old daemon"
$SSH "$TV" "[ -x $DEST/run.sh ] && sh $DEST/run.sh stop || true" </dev/null

echo "copying files to $TV:$DEST"
COPYFILE_DISABLE=1 tar czf - -C build grokbotd -C .. scripts/run.sh "$IPK" config.example.json |
  $SSH "$TV" "mkdir -p $DEST && rm -rf $DEST/models && cd $DEST && tar xzof - 2>/dev/null && mv scripts/run.sh run.sh && rmdir scripts && chmod 755 grokbotd run.sh"

echo "installing overlay app"
$SSH "$TV" "cp $DEST/$IPK /tmp/grokbot-overlay.ipk && luna-send -n 6 -w 60000 luna://com.webos.appInstallService/dev/install '{\"id\":\"com.ares.defaultName\",\"ipkUrl\":\"/tmp/grokbot-overlay.ipk\",\"subscribe\":true}' </dev/null | grep -o '\"statusValue\":[0-9]*\|\"state\":\"[a-z]*\"' | tail -1; rm -rf $DEST/build" </dev/null

echo "config and boot hook"
$SSH "$TV" "mkdir -p $CONF_DIR && chmod 700 $CONF_DIR
if [ ! -f $CONF_DIR/config.json ]; then
  sed 's/\"xai_api_key\": \"\"/\"xai_api_key\": \"${XAI_API_KEY}\"/' $DEST/config.example.json > $CONF_DIR/config.json
  echo 'wrote $CONF_DIR/config.json'
fi
chmod 600 $CONF_DIR/config.json
mkdir -p /var/lib/webosbrew/init.d
printf '#!/bin/sh\n[ -x $DEST/run.sh ] && sh $DEST/run.sh start\nexit 0\n' > $HOOK
chmod 755 $HOOK" </dev/null

$SSH "$TV" "sh $DEST/run.sh start" </dev/null
echo "logs: ssh $TV tail -f /tmp/grokbot.log"
