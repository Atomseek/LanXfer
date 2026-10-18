#!/system/bin/sh
# Runs at late_start service stage as root.
MODDIR=$(cd "$(dirname "$0")" && pwd)
. "$MODDIR/common.sh"
until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 2; done
sleep 5
load_conf
if [ -f "$DATA/disable" ]; then
  set_desc "⏸ 服务已关闭。点「操作」开启服务。"
else
  start_service
fi
