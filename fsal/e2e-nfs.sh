#!/bin/sh
# Drives the path a client actually takes, and checks what comes back.
#
#   NFS client -> Ganesha -> FSAL -> unix socket -> agent -> tier -> backend
#
# Needs a Linux host with Ganesha's build dependencies, root, and an NFS
# client. It patches a Ganesha tree, builds it, serves an export, mounts it and
# compares bytes. Every fault in this corner was found by doing these steps by
# hand and missing one; this does the same ones every time.
#
#   ./e2e-nfs.sh /path/to/nfs-ganesha
set -eu

[ $# -eq 1 ] || { echo "usage: $0 <nfs-ganesha source tree>"; exit 2; }
ganesha=$1
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
sock=$work/fb.sock
mnt=$work/mnt
failed=0

cleanup() {
	umount -f -l "$mnt" 2>/dev/null || true
	killall -9 ganesha.nfsd 2>/dev/null || true
	killall -9 forebay-agent 2>/dev/null || true
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

# --- patch and build ------------------------------------------------------
cp "$root/fsal/forebay_client.h" "$root/fsal/forebay_client.c" "$ganesha/src/fsal/" 2>/dev/null ||
	{ mkdir -p "$ganesha/src/fsal"; cp "$root/fsal/forebay_client.h" "$root/fsal/forebay_client.c" "$ganesha/src/fsal/"; }
cp "$root/fsal/mem_forebay.c" "$root/fsal/mem_forebay.h" "$ganesha/src/FSAL/FSAL_MEM/"
sed -i 's|#include "../../fsal/forebay_client.h"|#include "forebay_client.h"|' \
	"$ganesha/src/FSAL/FSAL_MEM/mem_forebay.c"
grep -q forebay "$ganesha/src/FSAL/FSAL_MEM/CMakeLists.txt" || {
	sed -i "1i include_directories($ganesha/src/fsal)" "$ganesha/src/FSAL/FSAL_MEM/CMakeLists.txt"
	sed -i "s|  mem_export.c|  mem_export.c\n  mem_forebay.c\n  $ganesha/src/fsal/forebay_client.c|" \
		"$ganesha/src/FSAL/FSAL_MEM/CMakeLists.txt"
}
python3 "$root/fsal/patch_mem_read2.py" "$ganesha/src/FSAL/FSAL_MEM/mem_handle.c"

mkdir -p "$ganesha/src/build"
cd "$ganesha/src/build"
cmake .. -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_STANDARD_LIBRARIES=-lacl \
	-DUSE_FSAL_CEPH=OFF -DUSE_FSAL_RGW=OFF -DUSE_FSAL_GLUSTER=OFF -DUSE_FSAL_LUSTRE=OFF \
	-DUSE_FSAL_GPFS=OFF -DUSE_FSAL_KVSFS=OFF -DUSE_FSAL_PROXY_V4=OFF -DUSE_FSAL_PROXY_V3=OFF \
	-DUSE_9P=OFF -DUSE_NFS3=OFF -DUSE_RQUOTA=OFF -DUSE_GSS=OFF >"$work/cmake.log" 2>&1
make -j"$(nproc)" >"$work/build.log" 2>&1
make install >/dev/null 2>&1
ldconfig
cd "$root"

# --- the object, and what it should read back as --------------------------
go build -o "$work/forebay-agent" "$root/cmd/forebay-agent"
# Rebuilt rather than reused: a binary left by another machine is newer than
# the sources and make would keep it.
make -s -C "$root/fsal" clean >/dev/null
make -s -C "$root/fsal" >/dev/null
mkdir -p "$work/backend" "$mnt"
dd if=/dev/urandom of="$work/backend/shard" bs=1024 count=9001 2>/dev/null
size=$(wc -c < "$work/backend/shard" | tr -d ' ')
expected=$("$root/fsal/forebay-client-check" --sum "$work/backend/shard")

"$work/forebay-agent" --borrowed-dir="$work/borrowed" --journal="$work/state/leases.json" \
	--capacity-bytes=2147483648 --reserved-bytes=0 --serve-socket="$sock" \
	--backend-dir="$work/backend" --tier-bytes=134217728 --tier-block-bytes=1048576 \
	>"$work/agent.log" 2>&1 &
i=0; while [ ! -S "$sock" ]; do i=$((i+1)); [ $i -gt 100 ] && { cat "$work/agent.log"; exit 1; }; sleep 0.1; done

# --- serve and mount ------------------------------------------------------
mkdir -p /etc/ganesha /var/run/ganesha
cat > /etc/ganesha/ganesha.conf <<CONF
NFS_CORE_PARAM { Protocols = 4; NFS_Port = 2049; }
NFSV4 { Minor_Versions = 1,2; Graceless = true; }
MDCACHE { Dir_Chunk = 0; }
EXPORT { Export_Id = 1; Path = /; Pseudo = /fb; Access_Type = RW;
         Squash = No_Root_Squash; SecType = sys; FSAL { Name = MEM; } }
LOG { Default_Log_Level = EVENT; }
CONF
FOREBAY_SOCKET=$sock "$ganesha/src/build/ganesha.nfsd" \
	-f /etc/ganesha/ganesha.conf -L "$work/ganesha.log" -N NIV_EVENT \
	-p /var/run/ganesha/ganesha.pid
sleep 5
# soft, so a wedged export cannot take the server with it and leave threads in
# uninterruptible IO on the mount they serve.
mount -t nfs4 -o vers=4.1,noresvport,soft,timeo=50,retrans=2 127.0.0.1:/fb "$mnt"

# --- what a client sees ---------------------------------------------------
truncate -s "$size" "$mnt/shard"
timeout 120 dd if="$mnt/shard" of="$work/got.bin" bs=1M 2>/dev/null || true
want "the bytes a client reads are the object's" \
	"$("$root/fsal/forebay-client-check" --sum "$work/got.bin")" "$expected"

# A file with no object behind it, and the same with the agent stopped: both
# must fail rather than hand back this FSAL's padding as file contents.
#
# On the exit status, not just the byte count. A read that hangs until timeout
# kills it also leaves an empty file, so counting bytes alone calls a wedged
# export a pass.
read_verdict() {
	rc=0
	timeout 60 dd if="$1" of="$2" bs=4096 count=1 2>/dev/null || rc=$?
	case "$rc" in
	0) echo served ;;
	124) echo hung ;;
	*) echo failed ;;
	esac
}

truncate -s 4096 "$mnt/nobacking"
want "a file with no object behind it fails the read" \
	"$(read_verdict "$mnt/nobacking" "$work/nb.bin")" "failed"
want "and hands back no bytes" \
	"$(wc -c < "$work/nb.bin" | tr -d ' ')" "0"

# With the agent still up, so this is about the read path and not about the
# backend being gone. A read that jumps past its own completion leaves a share
# reservation held, and every later read on the export then blocks.
#
# Read the good file again, through the same export, and check the bytes are
# still right. Not a listing: READDIR on a non-empty MEM export hangs on stock
# Ganesha, with this FSAL's hook removed entirely, so it would be measuring
# that and not this.
timeout 120 dd if="$mnt/shard" of="$work/again.bin" bs=1M 2>/dev/null || true
want "a failed read does not wedge the export" \
	"$("$root/fsal/forebay-client-check" --sum "$work/again.bin")" "$expected"

killall -9 forebay-agent 2>/dev/null || true
sleep 2
truncate -s 4096 "$mnt/fresh"
want "with the agent stopped, a fresh read fails" \
	"$(read_verdict "$mnt/fresh" "$work/fr.bin")" "failed"
want "and hands back no bytes" \
	"$(wc -c < "$work/fr.bin" | tr -d ' ')" "0"

echo
if [ "$failed" -eq 0 ]; then echo "end to end over NFS: everything checked out"
else echo "end to end over NFS: $failed checks failed"; fi
exit "$failed"
