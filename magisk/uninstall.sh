#!/system/bin/sh
MODDIR=$(cd "$(dirname "$0")" && pwd)
. "$MODDIR/common.sh"
stop_service
rm -rf /data/adb/lanxfer
