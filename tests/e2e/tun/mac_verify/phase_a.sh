#!/bin/zsh
BIN=/tmp/phaethon-verify-bin
DIR=/Users/cmll/phaethon-test
LOG=/tmp/verify-r1.log

echo "[PRECLEAN] kill all + wipe residual"
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 1
ps aux | grep phaethon-verify-bin | grep -v grep && { echo "ABORT: procs alive"; exit 1; }
for u in $(ifconfig -l | tr ' ' '\n' | grep '^utun'); do
  a=$(ifconfig $u 2>/dev/null | grep 'inet 100\.' | awk '{print $2}')
  [ -n "$a" ] && sudo ifconfig $u delete $a 2>/dev/null
done
netstat -rn -f inet | grep -E '^(0/1|128/1)' || echo "[PRECLEAN-OK] no split routes, no 100.x addr"

echo "[START]"
cd $DIR
sudo nohup $BIN > $LOG 2>&1 &
sleep 1
for i in {1..40}; do curl -sk --max-time 2 https://127.0.0.1:39999/api/tun >/dev/null 2>&1 && break; sleep 1; done
grep -c 'started ok' $LOG >/dev/null || echo "WARN: started marker not seen"

echo "[ENABLE]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":true}' https://127.0.0.1:39999/api/tun; echo
sleep 4
echo "[ROUTES-AFTER-ENABLE]"
netstat -rn -f inet | grep -E '^(0/1|128/1)'
UTUN_T=$(netstat -rn -f inet | grep '^0/1' | awk '{print $2}')
echo "iface=$UTUN_T addr=$(ifconfig $UTUN_T | grep 'inet ' | awk '{print $2}')"

echo "[DISABLE]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' https://127.0.0.1:39999/api/tun; echo
sleep 3
netstat -rn -f inet | grep -E '^(0/1|128/1)' && echo "FAIL: routes remain after disable" || echo "[DISABLE-ROUTES-PASS]"
ifconfig $UTUN_T 2>/dev/null | grep 'inet 100\.' && echo "FAIL: addr remains after disable" || echo "[DISABLE-ADDR-PASS]"
echo "PHASE-A DONE"
