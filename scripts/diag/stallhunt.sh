#!/bin/bash
# Stall hunter for the vm→qg→jf 8-stream mesh path.
#
# Runs 8 parallel scp pulls on the VM, watches the summed file sizes, and on a
# flat period (>6s without progress, not all files complete) captures the state
# of all three nodes plus a QG goroutine dump.
#
# Prereqs on the VM:
#   /c/temp/client-all.crt, /c/temp/client-all.key (admin client certs)
#   QG started with --enable-debug-api (for the goroutine dump)
#   the test package at $P on jf
#
# Usage: bash /c/temp/stallhunt.sh <run-id>
# Output: /c/temp/stall_report.txt on a stall.
R="${1:-1}"
P=/home/layer4/layer4-deploy/data/packages/e24a91806e6ac3759cf02632da148b27.pkg
K=/c/Users/Docker/.ssh/id_ed25519
C=/c/temp/client-all.crt
CK=/c/temp/client-all.key
SIZE=8178958
cd /c/temp || exit 1
rm -f "st${R}"?.bin /c/temp/stall_report.txt
for i in 1 2 3 4 5 6 7 8; do
  timeout 600 scp -O -q -P 22 -i "$K" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "layer4@jf.phn:$P" "/c/temp/st${R}${i}.bin" &
done
prev=-1; flat=0
while true; do
  sum=0; full=0; list=""
  for f in /c/temp/st${R}?.bin; do
    s=$(stat -c %s "$f" 2>/dev/null || echo 0)
    sum=$((sum+s)); list="$list $s"
    [ "$s" = "$SIZE" ] && full=$((full+1))
  done
  if [ "$full" = "8" ]; then echo "ALL_DONE R=$R"; break; fi
  if [ "$sum" = "$prev" ]; then flat=$((flat+1)); else flat=0; fi
  prev=$sum
  if [ "$flat" -ge 3 ]; then
    {
      echo "STALL $(date) R=$R sum=$sum files=$list"
      echo "===JF"
      curl -sk -m 8 --resolve jf.phn:39999:100.2.0.10 --cert "$C" --key "$CK" https://jf.phn:39999/api/mesh | tr ',' '\n' | grep -E 'retransmits|segments_sent|dsack|timeouts'
      echo "===JF_SSH"
      curl -sk -m 8 --resolve jf.phn:39999:100.2.0.10 --cert "$C" --key "$CK" https://jf.phn:39999/api/mesh | tr '}' '\n' | grep '"localPort":22' | head -6
      echo "===QG"
      curl -sk -m 8 --cert "$C" --key "$CK" https://qg.phn:39999/api/mesh | tr ',' '\n' | grep -E 'DataDrops|transitFastPath|uturnCount|meshInboundDrops|tunWriteDrops'
      echo "===QG_PEERS"
      curl -sk -m 8 --cert "$C" --key "$CK" https://qg.phn:39999/api/mesh | tr '}' '\n' | grep -oE '"nodeId":"(vm|jf)"[^}]*' | head -4
      echo "===VM"
      curl -sk -m 8 --cert "$C" --key "$CK" https://127.0.0.1:39999/api/mesh | tr ',' '\n' | grep -E 'DataDrops|meshInboundDrops|tunWriteDrops'
      echo "===GORO"
      curl -sk -m 10 --cert "$C" --key "$CK" "https://qg.phn:39999/debug/pprof/goroutine?debug=1" -o /c/temp/goro_stall.txt
      grep -c '^goroutine' /c/temp/goro_stall.txt
      grep '^goroutine' /c/temp/goro_stall.txt | sed 's/.*\[//; s/\].*//' | sed 's/, [0-9]* minutes//' | sort | uniq -c | sort -rn | head -12
    } > /c/temp/stall_report.txt 2>&1
    echo "STALL_REPORTED R=$R"
    break
  fi
  sleep 2
done
wait
