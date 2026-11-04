#!/bin/zsh
BIN=/tmp/phaethon-verify-bin
DIR=/Users/cmll/phaethon-test
LOG=/tmp/verify-r2.log

echo "[PRECLEAN]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' https://127.0.0.1:39999/api/tun 2>/dev/null; echo
sleep 3
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 1
ps aux | grep phaethon-verify-bin | grep -v grep && { echo "ABORT: procs alive"; exit 1; }
for u in $(ifconfig -l | tr ' ' '\n' | grep '^utun'); do
  a=$(ifconfig $u 2>/dev/null | grep 'inet 100\.' | awk '{print $2}')
  [ -n "$a" ] && sudo ifconfig $u delete $a 2>/dev/null
done
netstat -rn -f inet | grep -E '^0/1|^128' || echo "[PRECLEAN-OK]"

cd $DIR
sudo nohup $BIN > $LOG 2>&1 &
sleep 1
for i in {1..40}; do curl -sk --max-time 2 https://127.0.0.1:39999/api/tun >/dev/null 2>&1 && break; sleep 1; done

echo "[ENABLE]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":true}' https://127.0.0.1:39999/api/tun; echo
sleep 4
echo "[ROUTES-AFTER-ENABLE]"
netstat -rn -f inet | grep -E '^0/1|^128'
UTUN_T=$(netstat -rn -f inet | grep '^0/1' | awk '{print $2}')
echo "$UTUN_T" > /tmp/verify-utun.txt
echo "tun iface=$UTUN_T"

echo "[KILL-9-ALL]"
sudo pkill -9 -f phaethon-verify-bin; sleep 2
sudo pkill -9 -f phaethon-verify-bin; sleep 1
ps aux | grep phaethon-verify-bin | grep -v grep && echo "FAIL: procs alive" || echo "[ALL-KILLED]"
echo "[RESIDUAL-AFTER-KILL9]"
netstat -rn -f inet | grep -E '^0/1|^128'
ifconfig $UTUN_T 2>/dev/null | grep 'inet 100\.'

echo "[RESTART-FOR-CLEANUP]"
cd $DIR
sudo nohup $BIN > $LOG 2>&1 &
sleep 1
for i in {1..40}; do curl -sk --max-time 2 https://127.0.0.1:39999/api/tun >/dev/null 2>&1 && break; sleep 1; done
sleep 8
echo "[CLEANUP-LOG]"; grep -iE 'cleanup|skip interface' $LOG || echo "(no cleanup lines in nohup log)"
grep -iE 'cleanup|skip interface' ~/phaethon-test/data/logs/phaethon.log 2>/dev/null | tail -5
echo "[OLD-UTUN-AFTER-CLEANUP]"; ifconfig $UTUN_T 2>/dev/null | grep 'inet ' || echo "PASS: $UTUN_T has no addr (cleanup cleared it)"
echo "[ROUTES-AFTER-RESTART]"; netstat -rn -f inet | grep -E '^0/1|^128'
echo "[TUN-STATE]"; curl -sk --max-time 3 https://127.0.0.1:39999/api/tun | grep -o '"running":[a-z]*'

echo "[AUTO-CLEANUP] disable + kill all"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' https://127.0.0.1:39999/api/tun 2>/dev/null; echo
sleep 4
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 1
ps aux | grep phaethon-verify-bin | grep -v grep || echo "[PROCS-GONE]"
netstat -rn -f inet | grep -E '^0/1|^128' || echo "[ROUTES-GONE]"
ifconfig | grep 'inet 100\.' || echo "[100ADDR-GONE]"
ping -c 1 -t 3 192.168.1.101 >/dev/null 2>&1 && echo "[NET-OK]" || echo "[NET-FAIL]"
echo "PHASE-B DONE"
