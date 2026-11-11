#!/bin/sh
# verify-worker.sh <deploy-dir> [binary-path]
#
# Verifies that the process actually serving traffic is the binary that was
# just deployed:
#   1. no worker-pin overrides a different version
#   2. at least one running phaethon process resolves to <binary-path>
#
# Exit: 0 ok, 1 pin override, 2 bad args/binary, 3 no matching worker.
set -u
dir=${1:-.}
bin=${2:-$dir/phaethon}
cd "$dir" || { echo "FATAL: cd $dir failed"; exit 2; }
[ -x "$bin" ] || { echo "FATAL: binary not executable: $bin"; exit 2; }

pin=$(cat data/state/worker-pin 2>/dev/null | tr -d '[:space:]')
ver=$("$bin" --version 2>/dev/null | sed -n 's/^version=//p')
echo "deployed=$ver pin=${pin:-none}"
status=0
if [ -n "$pin" ] && [ "$pin" != "$ver" ]; then
  echo "WARN: worker-pin=$pin overrides deployed=$ver; clear data/state/worker-pin"
  status=1
fi

md5_of() {
  if command -v md5sum >/dev/null 2>&1; then
    md5sum "$1" 2>/dev/null | awk '{print $1}'
  else
    md5 -q "$1" 2>/dev/null
  fi
}
deployed_md5=$(md5_of "$bin")
found=0
for p in $(pgrep -x phaethon 2>/dev/null); do
  if [ -r "/proc/$p/exe" ]; then
    exe=$(readlink "/proc/$p/exe" 2>/dev/null)
  else
    exe=$(lsof -a -p "$p" -d txt -Fn 2>/dev/null | sed -n 's/^n//p' | head -1)
  fi
  [ -n "$exe" ] || continue
  echo "pid=$p exe=$exe"
  if [ "$exe" = "$bin" ] || [ "$(md5_of "$exe")" = "$deployed_md5" ]; then
    found=$((found + 1))
  fi
done
if [ "$found" -eq 0 ]; then
  echo "FATAL: no running worker matches $bin (md5 $deployed_md5)"
  exit 3
fi
echo "OK: $found process(es) match the deployed binary"
exit $status
