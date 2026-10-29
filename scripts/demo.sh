#!/usr/bin/env bash
# Recorded demo: sysgate serving real gVisor sandboxes end to end.
set -euo pipefail

B=http://127.0.0.1:18082
STATE=/data/sysgate/.runsc

step() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

run() {
  printf '\n\033[1;32m$\033[0m %s\n' "$1"
  bash -c "$1"
  sleep 1.2
}

printf '\033[2J\033[H'
step 'sysgate: warm gVisor sandboxes behind a tiny HTTP API'
run 'uname -sr'

step '1. Start the gateway with 2 pre-booted sandboxes'
sudo -n pkill -x sysgate 2>/dev/null || true
sleep 6
while read -r id _; do
  [ -n "$id" ] || continue
  sudo -n runsc --root "$STATE" kill "$id" KILL >/dev/null 2>&1 || true
  sudo -n runsc --root "$STATE" delete -force "$id" >/dev/null 2>&1 || true
done < <(sudo -n runsc --root "$STATE" list 2>/dev/null | awk 'NR>1')
run 'sudo -n env SYSGATE_DRIVER=runsc SYSGATE_ROOTFS=/data/gvisor-rootfs SYSGATE_ROOT=/data/sysgate/.runsc SYSGATE_LISTEN=127.0.0.1:18082 SYSGATE_POOL_SIZE=2 setsid ./target/release/sysgate >/tmp/sysgate-demo.log 2>&1 < /dev/null &'
for _ in $(seq 1 60); do
  warm=$(curl -s --max-time 1 "$B/metrics" 2>/dev/null | awk '/^sysgate_pool_warm /{print $2}') || warm=0
  [ "$warm" = "2" ] && break
  sleep 0.3
done
run "curl -s $B/healthz; echo"
run "curl -s $B/metrics | grep '^sysgate_pool_warm '"

step '2. Create a sandbox: served instantly from the warm pool'
run "curl -s -X POST $B/v1/sandboxes | tee /tmp/sysgate-create.json | jq ."
ID=$(jq -r .id /tmp/sysgate-create.json)
export ID

step '3. Execute inside the sandbox'
run "curl -s -X POST $B/v1/sandboxes/\$ID/exec -H 'content-type: application/json' -d '{\"argv\":[\"/bin/echo\",\"hello from gVisor\"]}' | jq -c ."
run "curl -s -X POST $B/v1/sandboxes/\$ID/exec -H 'content-type: application/json' -d '{\"argv\":[\"/bin/sh\",\"-c\",\"uname -sr\"]}' | jq -c ."

step '4. Proof: the workload runs on the gVisor kernel, not the host'
run "curl -s -X POST $B/v1/sandboxes/\$ID/exec -H 'content-type: application/json' -d '{\"argv\":[\"/bin/cat\",\"/proc/version\"]}' | jq -r .stdout"

step '5. Exit codes pass through'
run "curl -s -X POST $B/v1/sandboxes/\$ID/exec -H 'content-type: application/json' -d '{\"argv\":[\"/bin/sh\",\"-c\",\"exit 42\"]}' | jq -c ."

step '6. Metrics'
run "curl -s $B/metrics | grep -E '^sysgate_(pool_warm|pool_hits_total|pool_misses_total|sandboxes_created_total|sandboxes_destroyed_total|exec_total) '"

step '7. Delete: destroy the sandbox, refill the pool'
run "curl -s -o /dev/null -w 'HTTP %{http_code}\n' -X DELETE $B/v1/sandboxes/\$ID"
sleep 3
run "sudo -n runsc --root $STATE list | awk 'NR==1 {print \$1, \$3} NR>1 {print \$1, \$3}'"

step '8. Shutdown: every warm sandbox is cleaned up'
run 'sudo -n pkill -x sysgate'
sleep 6
run "sudo -n runsc --root $STATE list"

printf '\n\033[1;32mAll clean: sysgate + gVisor, end to end.\033[0m\n'
