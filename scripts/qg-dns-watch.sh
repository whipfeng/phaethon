#!/bin/ash
LOG=/root/dns-watch.log
echo "=== watcher started at $(date '+%Y-%m-%d %H:%M:%S') ===" > $LOG
LAST=$(stat -c '%Y' /etc/resolv.conf 2>/dev/null)
echo "initial mtime=$(stat -c '%y' /etc/resolv.conf 2>/dev/null) content=$(cat /etc/resolv.conf)" >> $LOG
while true; do
  CUR=$(stat -c '%Y' /etc/resolv.conf 2>/dev/null)
  if [ "$CUR" != "$LAST" ]; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') CHANGED" >> $LOG
    echo "  resolv.conf: $(cat /etc/resolv.conf)" >> $LOG
    [ -f /etc/resolv.conf.phaethon.bak ] && echo "  bak: $(cat /etc/resolv.conf.phaethon.bak)" >> $LOG
    LAST=$CUR
  fi
  sleep 30
done