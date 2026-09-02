#!/bin/sh
# Drives the whole read path and checks what comes back.
#
# Exists because every fault found in this corner was found by a check written
# once by hand and then thrown away. This one runs the same way every time.
#
#   agent  ->  unix socket  ->  C client  ->  bytes, compared to the source
#
# The NFS half needs Ganesha and a patched FSAL, so it is e2e-nfs.sh and runs
# on a node rather than here.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
agent=$work/forebay-agent
check=$root/fsal/forebay-client-check
sock=$work/fb.sock
failed=0

cleanup() {
	[ -n "${agent_pid:-}" ] && kill "$agent_pid" 2>/dev/null || true
	# Kept when something failed: a test that tidies away the logs leaves
	# nothing to look at, which is how this corner got debugged by hand in
	# the first place.
	if [ "$failed" -eq 0 ]; then rm -rf "$work"; else
		echo "logs kept in $work"
	fi
}
trap cleanup EXIT

say() { printf '%-56s %s\n' "$1" "$2"; }
want() {
	if [ "$2" = "$3" ]; then say "$1" "ok"; else
		say "$1" "FAILED: want $3, got $2"
		failed=$((failed + 1))
	fi
}

go build -o "$agent" "$root/cmd/forebay-agent"
# Rebuilt rather than reused: a binary left by another machine is newer than
# the sources and make would keep it.
make -s -C "$root/fsal" clean >/dev/null
make -s -C "$root/fsal" >/dev/null

mkdir -p "$work/backend"
# Not a round number of blocks, so the last one is short and the tail path is
# exercised rather than skipped.
dd if=/dev/urandom of="$work/backend/shard" bs=1024 count=9001 2>/dev/null
size=$(wc -c < "$work/backend/shard" | tr -d ' ')
expected=$("$check" --sum "$work/backend/shard")

"$agent" --borrowed-dir="$work/borrowed" --journal="$work/state/leases.json" \
	--capacity-bytes=536870912 --reserved-bytes=0 \
	--serve-socket="$sock" --backend-dir="$work/backend" \
	--tier-bytes=67108864 --tier-block-bytes=1048576 >"$work/agent.log" 2>&1 &
agent_pid=$!

# Wait for the socket rather than sleeping, so a slow machine does not fail
# and a fast one does not wait.
i=0
while [ ! -S "$sock" ]; do
	i=$((i + 1))
	[ "$i" -gt 100 ] && { cat "$work/agent.log"; echo "agent never listened"; exit 1; }
	sleep 0.1
done

echo "reading $size bytes through the agent, three times over"
# Three passes: the first fetches, the second admits, the third can hit, so a
# tier that serves the wrong bytes shows up rather than never being used.
pass=1
while [ "$pass" -le 3 ]; do
	out=$("$check" "$sock" shard "$size" "$expected" 2>&1) || true
	echo "$out" | sed "s/^/  pass $pass: /"
	echo "$out" | grep -q "^0 failed" || failed=$((failed + 1))
	pass=$((pass + 1))
done

# The tier must have been used, or the passes above only tested the backend.
grep -q "answering reads" "$work/agent.log" ||
	{ echo "the agent never reported serving"; failed=$((failed + 1)); }

echo
echo "restarting the agent, which replays the tier's lease from the journal"
kill "$agent_pid" 2>/dev/null || true
wait "$agent_pid" 2>/dev/null || true
"$agent" --borrowed-dir="$work/borrowed" --journal="$work/state/leases.json" \
	--capacity-bytes=536870912 --reserved-bytes=0 \
	--serve-socket="$sock" --backend-dir="$work/backend" \
	--tier-bytes=33554432 --tier-block-bytes=1048576 >"$work/agent2.log" 2>&1 &
agent_pid=$!
i=0
while [ ! -S "$sock" ]; do
	i=$((i + 1))
	[ "$i" -gt 100 ] && { cat "$work/agent2.log"; echo "the restarted agent never listened"; exit 1; }
	sleep 0.1
done
out=$("$check" "$sock" shard "$size" "$expected" 2>&1) || true
echo "$out" | sed 's/^/  after restart: /'
echo "$out" | grep -q "^0 failed" || failed=$((failed + 1))

echo
if [ "$failed" -eq 0 ]; then
	echo "end to end: everything checked out"
else
	echo "end to end: $failed checks failed"
fi
exit "$failed"
