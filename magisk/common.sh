# Shared helpers for service.sh / action.sh. Sourced, not executed.
MODDIR=${MODDIR:-${0%/*}}
DATA=${LANXFER_DATA:-/data/adb/lanxfer}
CONF=$DATA/config
RUN=$DATA/run
LOG=$DATA/lanxferd.log
BIN=$MODDIR/bin/lanxferd
mkdir -p "$RUN"

load_conf() {
  PORT=5740; TOKEN=""; RAW=1; WAKELOCK=1; NAME=""
  [ -f "$CONF" ] && . "$CONF"
}

sup_pid() { cat "$RUN/supervisor.pid" 2>/dev/null; }
is_running() {
  p=$(sup_pid)
  [ -n "$p" ] && kill -0 "$p" 2>/dev/null
}

set_desc() {
  # show the state in the Magisk module list
  sed -i "s|^description=.*|description=$1|" "$MODDIR/module.prop" 2>/dev/null
}

ips() {
  ip -4 -o addr show 2>/dev/null | awk '{print $2" "$4}' | grep -v -E '^(lo|rmnet|ccmni|dummy|tun)' | sed 's|/.*||'
}

# supervisor: restarts the daemon if it ever dies; both are shielded from the low-memory killer.
supervise() {
  echo $$ > "$RUN/supervisor.pid"
  load_conf
  echo -1000 > /proc/self/oom_score_adj 2>/dev/null
  [ "$WAKELOCK" = "1" ] && [ -e /sys/power/wake_lock ] && echo lanxfer > /sys/power/wake_lock
  while [ ! -f "$DATA/disable" ]; do
    RAWFLAG=true; [ "$RAW" = "1" ] || RAWFLAG=false
    "$BIN" -port "$PORT" -token "$TOKEN" -raw=$RAWFLAG ${NAME:+-name "$NAME"} >>"$LOG" 2>&1 &
    dpid=$!
    echo $dpid > "$RUN/daemon.pid"
    echo -1000 > /proc/$dpid/oom_score_adj 2>/dev/null
    wait $dpid
    [ -f "$DATA/disable" ] && break
    echo "$(date) lanxferd exited, restarting in 2s" >>"$LOG"
    sleep 2
    # keep the log small
    [ $(wc -c <"$LOG" 2>/dev/null || echo 0) -gt 1048576 ] && tail -c 262144 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
  done
  [ -e /sys/power/wake_unlock ] && echo lanxfer > /sys/power/wake_unlock
  rm -f "$RUN/supervisor.pid" "$RUN/daemon.pid"
}

start_service() {
  is_running && return 0
  rm -f "$DATA/disable"
  # fully detached from the caller (action.sh / service.sh)
  setsid sh -c "MODDIR='$MODDIR'; . '$MODDIR/common.sh'; supervise" </dev/null >/dev/null 2>&1 &
  sleep 1
  set_desc "✅ 服务运行中（端口 $PORT）。点「操作」关闭服务 / 查看令牌。"
}

stop_service() {
  touch "$DATA/disable"
  p=$(sup_pid); [ -n "$p" ] && kill "$p" 2>/dev/null
  d=$(cat "$RUN/daemon.pid" 2>/dev/null); [ -n "$d" ] && kill "$d" 2>/dev/null
  killall lanxferd 2>/dev/null
  [ -e /sys/power/wake_unlock ] && echo lanxfer > /sys/power/wake_unlock
  rm -f "$RUN/supervisor.pid" "$RUN/daemon.pid"
  set_desc "⏸ 服务已关闭。点「操作」开启服务。"
}
