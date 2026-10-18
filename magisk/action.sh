#!/system/bin/sh
# Magisk "Action" button: toggles the service and shows how to connect.
MODDIR=$(cd "$(dirname "$0")" && pwd)
. "$MODDIR/common.sh"
load_conf
if is_running; then
  stop_service
  echo "⏸ 服务已关闭"
else
  start_service
  if is_running; then
    echo "✅ 服务已开启"
  else
    echo "❌ 启动失败，日志: $LOG"
    tail -n 20 "$LOG" 2>/dev/null
    exit 1
  fi
fi
echo ""
echo "令牌: $TOKEN"
echo "端口: $PORT"
echo "手机 IP:"
ips | while read n a; do echo "  $a  ($n)"; done
echo ""
echo "电脑端 LanXfer 会自动搜索到本机；首次使用把令牌填进电脑端「设置」。"
