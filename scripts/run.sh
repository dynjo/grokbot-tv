#!/bin/sh
# grokbot supervisor, installed on the TV next to grokbotd.
#
#   run.sh start    start the supervisor loop in the background (idempotent)
#   run.sh stop     stop the loop and the daemon
#   run.sh status   "running pid=N" or "stopped"
#   run.sh          the loop itself: restarts grokbotd when it exits

# Boot hooks run with an almost empty PATH; grokbotd needs luna-send
# (/usr/bin) and systemctl (/bin).
export PATH=/usr/sbin:/usr/bin:/sbin:/bin:$PATH

DIR=$(cd "$(dirname "$0")" && pwd)
BIN="$DIR/grokbotd"
CONFIG=/home/root/.config/grokbot/config.json
LOG=/tmp/grokbot.log
LOOP_PID=/tmp/grokbot-run.pid
STOP=/tmp/grokbot.stop

alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null; }

case "$1" in
  start)
    rm -f "$STOP"
    if alive "$(cat "$LOOP_PID" 2>/dev/null)"; then
      echo "running pid=$(cat "$LOOP_PID")"
      exit 0
    fi
    setsid sh "$0" </dev/null >/dev/null 2>&1 &
    sleep 1
    echo "started pid=$(cat "$LOOP_PID" 2>/dev/null)"
    exit 0
    ;;
  stop)
    touch "$STOP"
    pid=$(cat "$LOOP_PID" 2>/dev/null)
    alive "$pid" && kill "$pid"
    pkill -f "^$BIN" 2>/dev/null
    # Give the daemon time to restore LG's voice assistant on SIGTERM.
    for i in 1 2 3 4 5; do pgrep -f "^$BIN" >/dev/null || break; sleep 1; done
    rm -f "$LOOP_PID"
    echo stopped
    exit 0
    ;;
  status)
    if alive "$(cat "$LOOP_PID" 2>/dev/null)"; then
      echo "running pid=$(cat "$LOOP_PID")"
    else
      echo stopped
    fi
    exit 0
    ;;
  "") ;;
  *)
    echo "usage: $0 [start|stop|status]"
    exit 2
    ;;
esac

echo $$ >"$LOOP_PID"
child=""
trap '[ -n "$child" ] && kill "$child" 2>/dev/null; rm -f "$LOOP_PID"; exit 0' TERM INT

# At power-on, wait for the app manager before starting.
i=0
while [ $i -lt 60 ] && ! luna-send -n 1 -w 2000 luna://com.webos.applicationManager/getForegroundAppInfo '{}' </dev/null >/dev/null 2>&1; do
  sleep 2
  i=$((i + 1))
done

delay=1
while [ ! -f "$STOP" ]; do
  # Keep the log small: /tmp is RAM.
  if [ -f "$LOG" ] && [ "$(wc -c <"$LOG")" -gt 1000000 ]; then
    mv -f "$LOG" "$LOG.1"
  fi
  started=$(date +%s)
  "$BIN" -config "$CONFIG" >>"$LOG" 2>&1 &
  child=$!
  wait "$child"
  child=""
  [ -f "$STOP" ] && break
  if [ $(($(date +%s) - started)) -gt 60 ]; then
    delay=1
  fi
  echo "run.sh: grokbotd exited, restarting in ${delay}s" >>"$LOG"
  sleep "$delay"
  delay=$((delay * 2))
  [ "$delay" -gt 60 ] && delay=60
done
rm -f "$LOOP_PID"
