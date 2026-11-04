#!/bin/zsh
BIN=/tmp/phaethon-verify-bin
DIR=/Users/cmll/phaethon-test
LOG=/tmp/verify-r3.log

echo "[PRECLEAN]"
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 1
for u in $(ifconfig -l | tr ' ' '\n' | grep '^utun'); do
  a=$(ifconfig $u 2>/dev/null | grep -E 'inet (100\.|198\.18\.)' | awk '{print $2}')
  [ -n "$a" ] && sudo ifconfig $u delete $a 2>/dev/null
done
sudo route delete -net 0/1 2>/dev/null; sudo route delete -net 198.18.0.0/16 2>/dev/null
netstat -rn -f inet | grep -E '^0/1|^128|^198\.18' || echo "[PRECLEAN-OK]"

cd $DIR
sudo nohup $BIN > $LOG 2>&1 &
sleep 1
for i in {1..40}; do curl -sk --max-time 2 https://127.0.0.1:39999/api/tun >/dev/null 2>&1 && break; sleep 1; done

echo "[ENABLE-DISABLE to leave orphan utun]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":true}' https://127.0.0.1:39999/api/tun >/dev/null; sleep 4
UTUN_T=$(netstat -rn -f inet | grep '^0/1' | awk '{print $2}')
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' https://127.0.0.1:39999/api/tun >/dev/null; sleep 3
echo "orphan iface=$UTUN_T"
echo "$UTUN_T" > /tmp/verify-utun.txt

echo "[INJECT 198.18 addr + 0/1 route on $UTUN_T]"
sudo ifconfig $UTUN_T alias 198.18.0.1 255.255.0.0
sudo route add -net 198.18.0.0/16 198.18.0.1
sudo route add -net 0/1 198.18.0.1
ifconfig $UTUN_T | grep 198.18 && echo "[INJECTED-ADDR]"
netstat -rn -f inet | grep -E '^198\.18' && echo "[INJECTED-ROUTE]"

echo "[RESTART -> cleanup must not touch 198.18]"
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
cd $DIR
sudo nohup $BIN > $LOG 2>&1 &
sleep 1
for i in {1..40}; do curl -sk --max-time 2 https://127.0.0.1:39999/api/tun >/dev/null 2>&1 && break; sleep 1; done
sleep 8
echo "[198.18 ADDR PRESERVED?]"
ifconfig $UTUN_T 2>/dev/null | grep 198.18 && echo "PASS: 198.18 addr intact" || echo "FAIL: 198.18 addr gone"
echo "[198.18 ROUTE PRESERVED?]"
netstat -rn -f inet | grep '^198\.18' && echo "PASS: 198.18 route intact" || echo "FAIL: 198.18 route gone"
echo "[INJECTED 0/1 PRESERVED?]"
netstat -rn -f inet | grep '^0/1'
echo "[CLEANUP LOG]"
grep -iE 'cleanup' $LOG || echo "(none in nohup log)"

echo "[FINAL-CLEANUP]"
curl -sk -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' https://127.0.0.1:39999/api/tun 2>/dev/null; echo
sleep 4
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 2
sudo pkill -9 -f phaethon-verify-bin 2>/dev/null; sleep 1
sudo ifconfig $UTUN_T -alias 198.18.0.1 2>/dev/null
sudo route delete -net 198.18.0.0/16 198.18.0.1 2>/dev/null
sudo route delete -net 0/1 198.18.0.1 2>/dev/null
sudo ifconfig $UTUN_T destroy 2>/dev/null
ps aux | grep phaethon-verify-bin | grep -v grep || echo "[PROCS-GONE]"
netstat -rn -f inet | grep -E '^0/1|^128|^198\.18' || echo "[ROUTES-GONE]"
ifconfig | grep -E 'inet (100\.|198\.18\.)' || echo "[ADDRS-GONE]"
ping -c 1 -t 3 192.168.1.101 >/dev/null 2>&1 && echo "[NET-OK]" || echo "[NET-FAIL]"
echo "PHASE-C DONE"
