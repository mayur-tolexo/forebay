"""Puts the Forebay read into Ganesha's memory FSAL.

Kept as a script rather than prose so the patch that gets tested is the patch
that gets described. Applying it twice is a no-op.
"""
import sys

path = sys.argv[1]
src = open(path).read()

if "mem_forebay_read" in src:
    print("already patched")
    sys.exit(0)

# A status the exit path can report. That label hands back ERR_FSAL_NO_ERROR
# whatever happened, so an error set anywhere above it is otherwise discarded.
anchor = """	fsal_status_t status = { ERR_FSAL_NO_ERROR, 0 }, status2;
	uint64_t offset = read_arg->offset;"""
assert anchor in src, "status declaration not found"
src = src.replace(anchor, """	fsal_status_t status = { ERR_FSAL_NO_ERROR, 0 }, status2;
	fsal_status_t fb_status = { ERR_FSAL_NO_ERROR, 0 };
	uint64_t offset = read_arg->offset;""", 1)

# Break out of the loop rather than jumping past its end: goto exit skips
# fsal_complete_io and the share counters, which leaves the reservation held
# and wedges the export.
anchor = """		if (offset < myself->datasize) {"""
assert anchor in src, "read loop not found"
src = src.replace(anchor, """		{
			size_t got = 0;
			enum mem_forebay_result fb;

			fb = mem_forebay_read(myself->m_name, offset,
					      read_arg->iov[i].iov_base,
					      bufsize, &got);
			if (fb == MEM_FOREBAY_SERVED && got == bufsize) {
				read_arg->io_amount += bufsize;
				offset += bufsize;
				continue;
			}
			if (fb != MEM_FOREBAY_ABSENT) {
				fb_status = fsalstat(ERR_FSAL_IO, EIO);
				break;
			}
		}
		if (offset < myself->datasize) {""", 1)

anchor = """	done_cb(obj_hdl, fsalstat(ERR_FSAL_NO_ERROR, 0), read_arg, caller_arg);"""
assert src.count(anchor) == 1, "the exit callback is not unique"
src = src.replace(anchor, """	done_cb(obj_hdl, fb_status, read_arg, caller_arg);""", 1)

src = src.replace('#include "mem_int.h"', '#include "mem_int.h"\n#include "mem_forebay.h"', 1)
open(path, "w").write(src)
print("patched")
