#!/bin/zsh
BIN=/tmp/phaethon-verify-bin
DIR=/Users/cmll/phaethon-test
LOG=/tmp/verify-r4.log

echo "[PRECLEAN]"
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null
sudo pkill -9 -f '/tmp/holder' 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null
sudo pkill -9 -f '/tmp/holder' 2>/dev/null; sleep 1
sudo route delete -net 0/1 198.18.0.1 2>/dev/null
sudo route delete -net 198.18.0.0/16 198.18.0.1 2>/dev/null
ps aux | grep -E 'phaethon-verify-bin|/tmp/holder' | grep -v grep && { echo "ABORT: procs alive"; exit 1; }
netstat -rn -f inet | grep -E '^0/1|^128|^198\.18' || echo "[PRECLEAN-OK]"

echo "[START-HOLDER]"
sudo nohup /tmp/holder > /tmp/holder.log 2>&1 &
sleep 2
cat /tmp/holder.log
HIF=$(grep -o 'utun[0-9]*' /tmp/holder.log | head -1)
echo "holder iface=$HIF"
[ -z "$HIF" ] && { echo "ABORT: holder not up"; exit 1; }

echo "[START-INSTANCE -> cleanup runs against holder]"
cd $DIR
sudo nohup $BIN > $LOG 2>&1 &
sleep 1
for i in {1..40}; do curl -sk --max-time 2 https://127.0.0.1:39999/api/tun >/dev/null 2>&1 && break; sleep 1; done
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":true}' https://127.0.0.1:39999/api/tun >/dev/null
sleep 5

echo "[HOLDER-UTUN-EXISTS?]"
ifconfig $HIF 2>/dev/null | grep -E '^utun|198\.18' && echo "PASS: holder iface $HIF alive" || echo "FAIL: holder iface gone"
echo "[HOLDER-198.18-ADDR?]"
ifconfig $HIF 2>/dev/null | grep 198.18 && echo "PASS: 198.18 addr intact" || echo "FAIL: 198.18 addr gone"
echo "[HOLDER-ROUTES?]"
netstat -rn -f inet | grep -E '^198\.18' && echo "PASS: 198.18 route intact" || echo "FAIL: 198.18 route gone"
netstat -rn -f inet | grep '^0/1'
echo "[INSTANCE-TUN-OK?]"
netstat -rn -f inet | grep -E '^0/1|^128'
curl -sk --max-time 3 https://127.0.0.1:39999/api/tun | grep -o '"running":[a-z]*'
echo "[CLEANUP-LOG]"
grep -iE 'cleanup' $LOG || echo "(none)"

echo "[FINAL-CLEANUP]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' https://127.0.0.1:39999/api/tun 2>/dev/null; echo
sleep 4
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
sudo pkill -9 -f '/tmp/holder' 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null
sudo pkill -9 -f '/tmp/holder' 2>/dev/null; sleep 1
sudo route delete -net 0/1 198.18.0.1 2>/dev/null
sudo route delete -net 198.18.0.0/16 198.18.0.1 2>/dev/null
ps aux | grep -E 'phaethon-verify-bin|/tmp/holder' | grep -v grep || echo "[PROCS-GONE]"
netstat -rn -f inet | grep -E '^0/1|^128|^198\.18' || echo "[ROUTES-GONE]"
ifconfig | grep -E 'inet (100\.|198\.18\.)' || echo "[ADDRS-GONE]"
ping -c 1 -t 3 192.168.1.101 >/dev/null 2>&1 && echo "[NET-OK]" || echo "[NET-FAIL]"
echo "PHASE-D DONE"
