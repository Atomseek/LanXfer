# Magisk install script
SKIPUNZIP=0
case "$ARCH" in
  arm64) B=lanxferd-arm64 ;;
  arm)   B=lanxferd-arm ;;
  x64)   B=lanxferd-amd64 ;;
  *) abort "不支持的架构: $ARCH" ;;
esac
[ -f "$MODPATH/bin/$B" ] || abort "缺少 $B"
mv "$MODPATH/bin/$B" "$MODPATH/bin/lanxferd"
rm -f "$MODPATH"/bin/lanxferd-*
set_perm_recursive "$MODPATH" 0 0 0755 0644
set_perm "$MODPATH/bin/lanxferd" 0 0 0755
set_perm "$MODPATH/service.sh" 0 0 0755
set_perm "$MODPATH/action.sh" 0 0 0755

DATA=/data/adb/lanxfer
mkdir -p $DATA
if [ ! -f $DATA/config ]; then
  TOKEN=$(head -c 64 /dev/urandom | md5sum | cut -c1-12)
  cat > $DATA/config <<CONF
# LanXfer 配置，修改后在 Magisk 里点两次「操作」（关再开）生效
PORT=5740
TOKEN=$TOKEN
# 1 = /sdcard 直接读写 /data/media/0，绕过 FUSE（更快）；0 = 走普通 /sdcard
RAW=1
# 1 = 服务开启时持有 wakelock，息屏也不降速（更耗电）；0 = 不持有
WAKELOCK=1
# 电脑上显示的设备名，留空 = 手机型号
NAME=
CONF
  chmod 600 $DATA/config
fi
. $DATA/config
ui_print "- 令牌: $TOKEN"
ui_print "- 重启后服务自动开启；在 Magisk 点本模块「操作」可开关服务"
