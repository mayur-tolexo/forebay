# Reading through Forebay from an NFS client

RFC-0008 puts a data server on the node and has an NFS server ask it. This is
the asking half: a small C client for the protocol `internal/dataserver`
speaks, and enough of an FSAL to show a client reading through it.

## What is here

| File | What it is |
| --- | --- |
| `forebay_client.h`, `forebay_client.c` | The protocol's second implementation. One request, one reply, fixed header, no negotiation |
| `forebay_client_check.c` | Checks this client against a running agent, without an NFS server in the way |
| `mem_forebay.c`, `mem_forebay.h` | The read hook, and a spike: the namespace is the memory FSAL's, the bytes are Forebay's |

## Proving the client on its own

The protocol has two implementations and this is the second, so the thing
worth proving first is that they agree.

```
make
./forebay-client-check /tmp/fb.sock shard 33554432
```

Against an agent serving a 32 MiB object:

```
checksum of the whole object                         4277891772
a read past the end is a range error                 ok
a request with no tenant is refused                  ok
an object that is not there is a failure             ok
the conversation survived all of that                ok
0 failed
```

The checksum matches what the Go side computes over the same object, and the
three statuses round-trip, which is what an NFS server needs to give a client
different errors for a bad range and a backend that could not answer.

## Reading through NFS

`mem_forebay.c` and `mem_forebay.h` go into NFS-Ganesha's `FSAL_MEM`, and
`mem_read2` calls the hook before falling back to what the FSAL holds:

```c
{
        size_t got = 0;
        enum mem_forebay_result fb;

        fb = mem_forebay_read(myself->m_name, offset,
                              read_arg->iov[i].iov_base, bufsize, &got);
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
```

with `fb_status` declared beside `status`, and reported at the end, since that
label hands back `ERR_FSAL_NO_ERROR` whatever happened:

```c
done_cb(obj_hdl, fb_status, read_arg, caller_arg);
```

**Break out of the loop, do not jump past the end of it.** The first version
used `goto exit`, which skips `fsal_complete_io` and the share counters below
it. The reservation stayed held, the export wedged, and every later operation
on it blocked, including listing the directory. Ganesha's own threads then sat
in uninterruptible IO on the mount they were serving, so the process could not
be killed either.


The three answers matter more than the hook does. Falling back is right when
the object is not Forebay's and wrong when Forebay could not answer: this FSAL
fills a buffer with padding, so an agent that is down would hand a client
fabricated bytes as the file's contents, which is worse than an error because
nothing about it looks like one.

With `FOREBAY_SOCKET` pointing at the agent, a stock Linux 6.8 client mounting
that export reads Forebay's bytes:

| | Byte sum |
| --- | --- |
| The object on the backend | 4278984864 |
| Read over NFS | 4278984864 |
| What the memory FSAL would have returned | 3254779904 |
| A file the backend does not have, read over NFS | 397312, which is 4096 x 97, the FSAL's own padding |

The third row is the control: the bytes are Forebay's rather than the FSAL's.
The last two are the reason the hook has three answers. Before it did, both
returned 4096 bytes of the letter a, presented to the client as the file's
contents. Fabricated bytes are worse than an error because nothing about them
looks like one.

## Running it

| Command | Covers |
| --- | --- |
| `make check` at the top level | compiles the client; see below for what it does not |
| `fsal/e2e.sh` | agent, socket, C client, three passes and a restart, against an order-sensitive checksum |
| `fsal/e2e-nfs.sh <ganesha tree>` | all of that plus Ganesha, an export and a mount, on a Linux host with root |

Both scripts keep their working directory when something fails, because a test
that tidies away the logs leaves nothing to look at.

**A known gap the NFS script prints every run.** After a read returns
`NFS4ERR_IO`, the export stops answering. Ganesha logs the error as converted
and non-retryable, so the FSAL returned what it should; where it stalls after
that has not been chased down. It is reported rather than asserted, since this
is a spike grafted onto a namespace that is not Forebay's.

## What this is not

**One connection, one read at a time.** Every read takes a process-wide lock
around a single socket, so concurrent NFS reads queue behind one another. Every
number above was measured with one reader and none of them would show it. A
connection per export, or a small pool, is what a real one needs; this is
enough to answer whether a client can read through it at all.


The namespace is the memory FSAL's: a file is created to give a name a size,
and reading it fetches the object of that name. A real FSAL would carry its
own namespace and its own handles.

`make check` compiles `forebay_client.c` and its check, which need nothing but
libc. It does not compile `mem_forebay.c`, which includes Ganesha's headers,
and those are not vendored. So the half that is covered is the self-contained
one and the half that is not holds an FSAL API surface: the file that breaks
when Ganesha moves is the file nothing here compiles.

Neither is the check binary run by CI, since that needs a live agent. A
compile catches a second implementation rotting; it does not catch it drifting
from the first.
