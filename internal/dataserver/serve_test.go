package dataserver_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/filedriver"
	"github.com/mayur-tolexo/forebay/internal/dataserver"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
)

// overSocket serves an object and returns a client connected to it.
func overSocket(t *testing.T, object string, content []byte) *dataserver.Client {
	t.Helper()
	srv, _ := serve(t, object, content)
	return connect(t, srv)
}

// socketPath gives an address a unix socket can actually bind.
//
// The address is capped around a hundred bytes, well under what t.TempDir
// produces from a subtest name, and going over is a bind error rather than a
// truncation. Tests that used the long path passed or failed on how deeply
// they were nested.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// connect starts a server on a unix socket and dials it.
func connect(t *testing.T, srv *dataserver.Server) *dataserver.Client {
	t.Helper()
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ctx, l); err != nil {
			t.Errorf("serving: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })

	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{MaxReply: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestAReadCrossesTheWireIntact(t *testing.T) {
	content := pattern(3*blockSize + 40)
	c := overSocket(t, "obj", content)

	for _, r := range []struct{ off, length int64 }{
		{0, blockSize},
		{blockSize + 17, 2*blockSize + 5},
		{3 * blockSize, 40},
		{0, int64(len(content))},
	} {
		got, err := c.ReadRange("t1", "obj", r.off, r.length)
		if err != nil {
			t.Fatalf("reading %d from %d: %v", r.length, r.off, err)
		}
		if !bytes.Equal(got, content[r.off:r.off+r.length]) {
			t.Errorf("%d bytes from %d came back wrong", r.length, r.off)
		}
	}
}

func TestABadRangeAndAFailureAreDifferentOnTheWire(t *testing.T) {
	// The distinction this layer draws has to survive the crossing. An NFS
	// server in front owes a client different errors for the two, and cannot
	// tell them apart from a failed read alone.
	c := overSocket(t, "obj", pattern(blockSize))

	_, err := c.ReadRange("t1", "obj", 0, 4*blockSize)
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("a read past the end came back as %v, want a range status", err)
	}

	_, err = c.ReadRange("", "obj", 0, 16)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a request with no tenant came back as %v, want a refusal", err)
	}
}

func TestManyReadsShareOneConnection(t *testing.T) {
	// One connection is one conversation, so concurrent callers must not
	// interleave a request with somebody else's reply.
	content := pattern(4 * blockSize)
	c := overSocket(t, "obj", content)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i%4) * blockSize
			got, err := c.ReadRange("t1", "obj", off, blockSize)
			switch {
			case err != nil:
				errs <- err
			case !bytes.Equal(got, content[off:off+blockSize]):
				errs <- errors.New("a reader was given another reader's bytes")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestAFrameFromSomethingElseIsNotAnswered(t *testing.T) {
	// A socket somebody else's protocol reached must not have a length read
	// out of it. The connection closes, since the framing is already lost.
	srv, _ := serve(t, "obj", pattern(blockSize))
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [8]byte
	if n, err := conn.Read(buf[:]); err == nil {
		t.Errorf("got %d bytes back from a frame this does not speak", n)
	}
}

// framed writes a reply header with a declared length, followed by whatever
// body is given, which lets a test say things a real server would not.
func framed(conn net.Conn, status byte, declared uint64, body []byte) {
	head := make([]byte, 14)
	binary.BigEndian.PutUint32(head[0:], 0x46425259)
	head[4], head[5] = 1, status
	binary.BigEndian.PutUint64(head[6:], declared)
	conn.Write(head)
	conn.Write(body)
}

// answering runs a fake server that replies to each request with reply(),
// which is how a test produces a frame a real server never would.
func answering(t *testing.T, reply func(conn net.Conn)) string {
	t.Helper()
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
			reply(conn)
		}
	}()
	return addr
}

func TestAReplyLargerThanTheClientAcceptsIsRefusedBeforeItIsRead(t *testing.T) {
	// The length in a reply is a number the far side chose. A client that
	// sizes its allocation from it has handed over its memory, so the bound
	// has to hold against a server that declares more than was asked for,
	// not only against a caller asking for too much.
	addr := answering(t, func(conn net.Conn) {
		framed(conn, 0, 1<<40, nil)
	})
	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{
		MaxReply: blockSize, Exchange: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.ReadRange("t1", "obj", 0, 16)
	if err == nil {
		t.Fatal("a reply declaring a terabyte was accepted")
	}
	if !strings.Contains(err.Error(), "more than the") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	// The stream is unusable after that, since the declared bytes were never
	// read and there is no way to know how many are really there.
	if _, err := c.ReadRange("t1", "obj", 0, 16); !errors.Is(err, dataserver.ErrBroken) {
		t.Errorf("second read = %v, want the conversation reported over", err)
	}
}

func TestAClientWillNotAskForMoreThanItAccepts(t *testing.T) {
	// The other half, and the weaker one: a caller asking for more than this
	// client will take is stopped before anything is sent.
	srv, _ := serve(t, "obj", pattern(4*blockSize))
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{MaxReply: blockSize})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ReadRange("t1", "obj", 0, 2*blockSize); err == nil {
		t.Error("a read larger than the client's bound was accepted")
	}
	// Nothing was sent, so the conversation is still usable.
	if _, err := c.ReadRange("t1", "obj", 0, blockSize); err != nil {
		t.Errorf("a refusal that sent nothing ended the conversation: %v", err)
	}
}

func TestAFrameFromAVersionThisDoesNotKnowIsRefused(t *testing.T) {
	// Refused rather than read: a version is the one field that says the
	// layout after it may have changed, so guessing at the rest is how an
	// older reader invents a length from somebody else's bytes.
	addr := answering(t, func(conn net.Conn) {
		head := make([]byte, 14)
		binary.BigEndian.PutUint32(head[0:], 0x46425259)
		head[4], head[5] = 99, 0
		conn.Write(head)
	})
	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{
		MaxReply: 1 << 20, Exchange: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ReadRange("t1", "obj", 0, 16); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("a frame from version 99 came back as %v, want it refused by version", err)
	}
}

func TestAStatusThisClientDoesNotKnowIsStillReported(t *testing.T) {
	// A newer server answering an older client is the case this exists for,
	// and it is the worst moment to find a formatting bug in the only code
	// that names the status.
	addr := answering(t, func(conn net.Conn) {
		framed(conn, 42, 0, nil)
	})
	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{
		MaxReply: 1 << 20, Exchange: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.ReadRange("t1", "obj", 0, 16)
	if err == nil || !strings.Contains(err.Error(), "status(42)") {
		t.Errorf("an unknown status came back as %v, want it named", err)
	}
}

func TestEveryStatusNamesItself(t *testing.T) {
	// The names end up in an operator's log, and the unknown one is the only
	// place a number would otherwise appear bare.
	for s, want := range map[dataserver.Status]string{
		dataserver.StatusOK:      "ok",
		dataserver.StatusRange:   "out of range",
		dataserver.StatusRefused: "refused",
		dataserver.StatusFailed:  "failed",
		dataserver.Status(42):    "status(42)",
	} {
		if got := s.String(); got != want {
			t.Errorf("Status(%d) = %q, want %q", uint8(s), got, want)
		}
	}
}

func TestANameLongerThanTheProtocolCarriesIsRefused(t *testing.T) {
	// A name length is a field the other side fills in, so it is bounded on
	// the way out as well as on the way in.
	srv, _ := serve(t, "obj", pattern(blockSize))
	c := connect(t, srv)
	long := strings.Repeat("t", 2000)

	if _, err := c.ReadRange(long, "obj", 0, 16); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("an over-long tenant came back as %v, want it refused", err)
	}
	if _, err := c.ReadRange("t1", long, 0, 16); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("an over-long object came back as %v, want it refused", err)
	}
}

func TestServingStopsWhenTheContextDoes(t *testing.T) {
	// Both the empty case and the ordinary one. An NFS server holding an
	// idle connection is the steady state, not an edge case, so a shutdown
	// that waits out the idle deadline on it is a SIGTERM the agent does not
	// answer for minutes.
	for _, c := range []struct {
		name string
		open int
	}{
		{"with nothing connected", 0},
		{"with an idle connection", 1},
		{"with several idle connections", 8},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := serve(t, "obj", pattern(blockSize))
			addr := socketPath(t)
			l, err := net.Listen("unix", addr)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx, l) }()

			for i := 0; i < c.open; i++ {
				conn, err := net.Dial("unix", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			}
			// Give the conversations time to be accepted, so cancelling
			// meets them waiting on a request rather than before they start.
			time.Sleep(100 * time.Millisecond)

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("a cancelled serve returned %v, want a clean stop", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("serving did not stop when the context did")
			}
		})
	}
}

func TestAConversationThatLostItsPlaceIsOver(t *testing.T) {
	// A stream protocol cannot be resynchronised: once a reply has been read
	// short or long, every byte after it is at the wrong offset. Continuing
	// gets a magic mismatch and a reason to suspect the wrong thing.
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		// A reply claiming eight bytes while sending far more, which is what
		// a truncated or interrupted answer looks like from here. The excess
		// is longer than a header so the next read reaches a verdict rather
		// than waiting for bytes that will not come.
		head := make([]byte, 14)
		binary.BigEndian.PutUint32(head[0:], 0x46425259)
		head[4], head[5] = 1, 0
		binary.BigEndian.PutUint64(head[6:], 8)
		conn.Write(head)
		conn.Write(bytes.Repeat([]byte("B"), 64))
		<-make(chan struct{})
	}()

	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{MaxReply: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.ReadRange("t1", "o", 0, 8); err != nil {
		t.Fatalf("the first read failed before the stream was lost: %v", err)
	}
	if _, err := c.ReadRange("t1", "o", 0, 4); !errors.Is(err, dataserver.ErrBroken) {
		t.Errorf("second read = %v, want the conversation reported over", err)
	}
	// And the third is refused without touching the socket at all, which is
	// what stops a caller retrying down a stream that cannot recover.
	if _, err := c.ReadRange("t1", "o", 0, 4); !errors.Is(err, dataserver.ErrBroken) {
		t.Errorf("third read = %v, want the same answer without asking again", err)
	}
}

func TestConversationsPastTheCapAreClosedRatherThanQueued(t *testing.T) {
	// The far side is one NFS server, so a great many at once is a fault to
	// make visible rather than load to absorb. A refused connection is closed
	// at once, not held open waiting for a slot.
	srv, _ := serve(t, "obj", pattern(blockSize))
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	// Fill every slot without speaking, so each is held by a conversation
	// that will never ask anything. These are not probed: a slot that is
	// taken stays silent, and waiting on each would only measure the timeout.
	for i := 0; i < 96; i++ {
		conn, err := net.Dial("unix", addr)
		if err != nil {
			t.Fatalf("dialling %d: %v", i, err)
		}
		held = append(held, conn)
	}
	// Now the ones past the cap, which should be closed on arrival.
	closed := 0
	for i := 0; i < 8; i++ {
		conn, err := net.Dial("unix", addr)
		if err != nil {
			continue
		}
		held = append(held, conn)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var b [1]byte
		if _, err := conn.Read(b[:]); err == io.EOF {
			closed++
		}
	}
	if closed == 0 {
		t.Error("nothing past the cap was closed, so the cap did nothing")
	}
	t.Logf("%d of 8 connections past the cap were closed on arrival", closed)
}

func TestAServerThatStopsMidReplyDoesNotHoldTheCaller(t *testing.T) {
	// The caller here is an NFS server with a client of its own waiting on
	// it, so waiting for ever is not a neutral choice. A reply that stops
	// part-way also leaves the stream nowhere, so the conversation is over.
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		// Half a header, then nothing.
		conn.Write([]byte{0x46, 0x42, 0x52, 0x59, 1, 0})
		<-make(chan struct{})
	}()

	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{
		MaxReply: 1 << 20, Exchange: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	if _, err := c.ReadRange("t1", "o", 0, 8); err == nil {
		t.Fatal("a reply that never arrived was accepted")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the caller waited %s on a server that stopped", elapsed)
	}
	// And the conversation is over, since a reply read part-way leaves the
	// stream in the same place a mis-framed one does.
	if _, err := c.ReadRange("t1", "o", 0, 8); !errors.Is(err, dataserver.ErrBroken) {
		t.Errorf("second read = %v, want the conversation reported over", err)
	}
}

func TestDiallingSomethingThatIsNotThereFails(t *testing.T) {
	// The socket is created by the agent, so a client that starts first is
	// ordinary rather than exceptional, and it has to say so.
	if _, err := dataserver.Dial("unix", socketPath(t), dataserver.ClientConfig{MaxReply: 1 << 20}); err == nil {
		t.Error("dialling a socket that does not exist succeeded")
	}
}

func TestAClientConfigThatWaitsForNothingIsRefused(t *testing.T) {
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, cfg := range []dataserver.ClientConfig{
		{MaxReply: 0},
		{MaxReply: -1},
		{MaxReply: 1 << 20, Exchange: -time.Second},
	} {
		if _, err := dataserver.Dial("unix", addr, cfg); err == nil {
			t.Errorf("%+v was accepted", cfg)
		}
	}
}

func TestAMalformedRequestClosesTheConnectionWithoutAnswering(t *testing.T) {
	// The side that will send these is a C FSAL, which is the implementation
	// most likely to get the framing wrong. A reply to a frame this cannot
	// parse would land in the middle of whatever the far side thinks it is
	// reading, so the connection ends instead.
	srv, _ := serve(t, "obj", pattern(blockSize))
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	// A well-formed header, then the fields varied one at a time.
	header := func(version, op byte, tenantLen, objectLen uint16) []byte {
		h := make([]byte, 26)
		binary.BigEndian.PutUint32(h[0:], 0x46425259)
		h[4], h[5] = version, op
		binary.BigEndian.PutUint16(h[6:], tenantLen)
		binary.BigEndian.PutUint16(h[8:], objectLen)
		return h
	}
	for _, c := range []struct {
		name  string
		frame []byte
		// closed says whether the server can tell the frame is wrong. A
		// frame it has read whole and cannot parse is over; one that merely
		// stopped part way may still finish, so it is waited on and the idle
		// deadline is what ends it.
		closed bool
	}{
		{"a version this does not know", header(99, 1, 2, 3), true},
		{"an operation this does not speak", header(1, 42, 2, 3), true},
		{"a tenant longer than the protocol carries", header(1, 1, 2000, 3), true},
		{"an object longer than the protocol carries", header(1, 1, 2, 2000), true},
		{"a header that stops half way", header(1, 1, 2, 3)[:10], false},
		{"names that never arrive", append(header(1, 1, 2, 3), 'x'), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, err := net.Dial("unix", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write(c.frame); err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			var buf [1]byte
			n, err := conn.Read(buf[:])
			switch {
			case n != 0:
				t.Errorf("got %d bytes back, want nothing answered", n)
			case c.closed && err != io.EOF:
				t.Errorf("read gave %v, want the connection closed", err)
			case !c.closed && err == io.EOF:
				t.Error("an unfinished frame was closed rather than waited on")
			}
		})
	}
}

func TestAReplyNobodyTakesDoesNotHoldTheServerForEver(t *testing.T) {
	// The mirror of the idle read, and the one that was missed: a caller that
	// asks and never takes the answer blocks the write, which a read deadline
	// does not cover. A slot held that way is held until the process ends
	// rather than until the timeout.
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "obj"), pattern(512*blockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(fd)
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	// The write is bounded by the exchange budget, not the idle one: idle is
	// how long a caller may stay quiet, and this caller is not quiet, it is
	// refusing to take its answer.
	srv, err := dataserver.New(tier, back, dataserver.Config{
		Backend: "store", Exchange: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A megabyte, far past any socket buffer, from a caller that then stops
	// reading.
	req := make([]byte, 26+5)
	binary.BigEndian.PutUint32(req[0:], 0x46425259)
	req[4], req[5] = 1, 1
	binary.BigEndian.PutUint16(req[6:], 2)
	binary.BigEndian.PutUint16(req[8:], 3)
	binary.BigEndian.PutUint64(req[18:], 1<<20)
	copy(req[26:], "t1obj")
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}

	// The server fills the socket buffer and blocks on the rest. Once the
	// bound passes it gives up and closes, so draining reaches the end
	// rather than running out the clock.
	time.Sleep(time.Second)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var total int
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		total += n
		if err != nil {
			if total >= 1<<20 {
				// The write was blocked and then unblocked by this very
				// drain, which means the server waited for the caller rather
				// than giving up on it.
				t.Fatalf("the whole reply arrived (%d bytes), so the server waited instead of ending the conversation", total)
			}
			if err == io.EOF {
				return // The server gave up and closed, which is the point.
			}
			t.Fatalf("after %d bytes the read gave %v, want the server to have closed", total, err)
		}
	}
}

func TestAServerConfigThatWaitsForNothingIsRefused(t *testing.T) {
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	fd, err := filedriver.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(fd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataserver.New(tier, back, dataserver.Config{Backend: "s", Idle: -time.Second}); err == nil {
		t.Error("a negative idle bound was accepted")
	}
}

// slowBackend takes a moment per read, which is what a remote store is and
// what a local temporary directory is not.
type slowBackend struct {
	driver.Driver
	each time.Duration
}

func (s *slowBackend) ReadRange(ctx context.Context, o string, off, n int64) ([]byte, error) {
	time.Sleep(s.each)
	return s.Driver.ReadRange(ctx, o, off, n)
}

func TestTimeSpentWaitingIsNotTakenFromTimeToAnswer(t *testing.T) {
	// A caller that stays quiet and then asks is the ordinary rhythm of an
	// NFS server between bursts, and the first read after a quiet spell is
	// the one most likely to miss and go to the backend. Charging the wait
	// against the work gives the slowest request the smallest budget.
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "obj"), pattern(64*blockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(&slowBackend{Driver: fd, each: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: blockSize, FirstReadLimit: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer tier.Close()
	// A short wait bound and a generous one for the work, which is the shape
	// the defaults have.
	srv, err := dataserver.New(tier, back, dataserver.Config{
		Backend: "store", Idle: 200 * time.Millisecond, Exchange: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := socketPath(t)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	c, err := dataserver.Dial("unix", addr, dataserver.ClientConfig{
		MaxReply: 1 << 20, Exchange: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Quiet for most of the wait bound, then ask for something that takes
	// longer than what is left of it.
	time.Sleep(190 * time.Millisecond)
	const want = 128 << 10
	got, err := c.ReadRange("t1", "obj", 0, want)
	if err != nil {
		t.Fatalf("a caller that had just spoken was cut off: %v", err)
	}
	if len(got) != want {
		t.Errorf("got %d bytes, want %d", len(got), want)
	}
}
