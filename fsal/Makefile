# Builds the client and its check, which need nothing but libc, so the protocol
# can be proved against a running agent before any of it is loaded into an NFS
# server. `make check` at the top level runs this.
#
# mem_forebay.c is not here and cannot be: it includes Ganesha's headers, which
# are not vendored. That is the file most likely to break when Ganesha's API
# moves, and nothing here will notice.
CFLAGS ?= -O2 -Wall -Wextra -Werror -std=c11 -D_GNU_SOURCE

all: forebay-client-check

forebay-client-check: forebay_client.c forebay_client_check.c forebay_client.h
	$(CC) $(CFLAGS) -o $@ forebay_client.c forebay_client_check.c

clean:
	rm -f forebay-client-check

.PHONY: all clean
