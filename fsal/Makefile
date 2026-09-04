# Builds the client and its check, which need nothing but libc, so the protocol
# can be proved against a running agent before any of it is loaded into an NFS
# server. `make check` at the top level runs this.
#
# forebay_fsal.c and mem_forebay.c are not in `all`, and cannot be: they include
# Ganesha's headers, which are not vendored. Those are the files most likely to
# break when Ganesha's API moves, and nothing in `all` will notice.
#
# `make module GANESHA_SRC=...` builds the FSAL against a configured Ganesha
# source tree. Configured, not built: the headers and the generated config.h
# are all it needs, which is a cmake run rather than a compile of Ganesha.
CFLAGS ?= -O2 -Wall -Wextra -Werror -std=c11 -D_GNU_SOURCE

all: forebay-client-check forebay-path-check forebay-source-check

forebay-client-check: forebay_client.c forebay_client_check.c forebay_client.h
	$(CC) $(CFLAGS) -o $@ forebay_client.c forebay_client_check.c

# The namespace mapping needs nothing but libc either, which is the point of
# keeping it out of the module: the half most likely to disagree with the Go
# side is the half that can be checked here.
forebay-path-check: forebay_path.c forebay_path_check.c forebay_path.h
	$(CC) $(CFLAGS) -o $@ forebay_path.c forebay_path_check.c

# The connection holder needs no agent to be checked: what matters is what it
# does when there is nothing to talk to, which is the state a node is in while
# the agent is starting.
forebay-source-check: forebay_source.c forebay_client.c forebay_source_check.c forebay_source.h forebay_client.h
	$(CC) $(CFLAGS) -o $@ forebay_source.c forebay_client.c forebay_source_check.c -lpthread

# Run what can be run without an agent or an NFS server.
check: forebay-path-check forebay-source-check
	./forebay-path-check
	./forebay-source-check

clean:
	rm -f forebay-client-check forebay-path-check forebay-source-check libfsalforebay.so

# GANESHA_SRC points at a checkout whose src/build holds a generated config.h.
#   cmake -S <ganesha>/src -B <ganesha>/src/build
#
# -Wextra is not used: Ganesha's own headers do not compile under it, and this
# is not the place to argue with them.
GANESHA_SRC ?=
GANESHA_CFLAGS = -O2 -Wall -Werror -std=gnu11 -D_GNU_SOURCE -fPIC \
	-I$(GANESHA_SRC)/src/build/include \
	-I$(GANESHA_SRC)/src/include \
	-I$(GANESHA_SRC)/src/libntirpc/ntirpc

module: libfsalforebay.so

libfsalforebay.so: forebay_fsal.c forebay_path.c forebay_source.c forebay_client.c
	@test -n "$(GANESHA_SRC)" || { echo "set GANESHA_SRC to a configured Ganesha tree"; exit 1; }
	$(CC) $(GANESHA_CFLAGS) -shared -o $@ $^ -lpthread

# Every symbol this needs must be one Ganesha exports. RFC-0008 found the
# flexfiles helpers absent from that list, so an FSAL calling them links
# nowhere; this one calls none, and this is what proves it still does.
module-symbols: libfsalforebay.so
	@missing=0; \
	for sym in $$(nm -u libfsalforebay.so | awk '$$1=="U"{print $$2}' | grep -v '@'); do \
		grep -qE "^[[:space:]]*$$sym;" $(GANESHA_SRC)/src/MainNFSD/libganesha_nfsd.ver || \
			{ echo "not exported by Ganesha: $$sym"; missing=1; }; \
	done; \
	test $$missing -eq 0 && echo "every symbol this needs is one Ganesha exports"

.PHONY: all check clean module module-symbols
