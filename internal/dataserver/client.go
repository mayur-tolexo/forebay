package dataserver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client reads through a data server over the wire.
//
// It exists for tests and as the readable statement of the protocol. What
// will use it in production is a C FSAL inside an NFS server, which is why
// the framing has no feature this file needs more than a page to explain.
type Client struct {
	// mu serialises the conversation. One connection carries a request and
	// then its reply, so two callers sharing one would read each other's.
	mu   sync.Mutex
	conn net.Conn
	// max bounds a reply, since the length is a number the far side chose.
	max int64
	// exchange bounds one request and its reply.
	exchange time.Duration
	// broken records that this conversation lost its place in the stream.
	//
	// A stream protocol cannot be resynchronised: once a reply has been read
	// short or long, every byte after it is at the wrong offset. The server
	// closes a connection whose framing it cannot trust, and this is the same
	// decision on the other side, made explicitly because a client that keeps
	// asking gets a magic mismatch and a reason to suspect the wrong thing.
	broken error
}

// ErrBroken reports a conversation that cannot be continued. A caller that
// wants to keep reading has to dial again.
var ErrBroken = errors.New("dataserver: the connection lost its place in the stream")

// DefaultExchange is how long one request and its reply may take when nothing
// else says.
//
// Generous, because a miss is a fetch from a durable backend and the point of
// this layer is that the caller waits rather than fails. Finite, because a
// server that stops mid-reply otherwise holds the caller for ever, and the
// caller here is an NFS server with a client of its own waiting on it.
const DefaultExchange = 30 * time.Second

// ClientConfig is what a client needs beyond an address.
type ClientConfig struct {
	// MaxReply bounds a reply. It is the caller's number because it sizes
	// the caller's memory, and the length on the wire is one the far side
	// chose.
	MaxReply int64
	// Exchange bounds one request and its reply. Zero means DefaultExchange.
	Exchange time.Duration
}

// Dial connects to a data server.
func Dial(network, address string, cfg ClientConfig) (*Client, error) {
	switch {
	case cfg.MaxReply <= 0:
		return nil, fmt.Errorf("dataserver: a reply bound of %d accepts nothing", cfg.MaxReply)
	case cfg.Exchange < 0:
		return nil, fmt.Errorf("dataserver: an exchange bound of %s is not a duration to wait", cfg.Exchange)
	}
	if cfg.Exchange == 0 {
		cfg.Exchange = DefaultExchange
	}
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, fmt.Errorf("dataserver: dialling %s: %w", address, err)
	}
	return &Client{conn: conn, max: cfg.MaxReply, exchange: cfg.Exchange}, nil
}

// ReadRange asks for length bytes of object from offset.
//
// A status that is not OK comes back as an error naming which, so a caller
// can tell a bad range from a backend that could not answer without parsing
// a message.
func (c *Client) ReadRange(tenant, object string, offset, length int64) ([]byte, error) {
	if length > c.max {
		return nil, fmt.Errorf("dataserver: %d bytes is more than the %d this client accepts", length, c.max)
	}
	return c.exchangeOne(request{
		Op: opReadRange, Tenant: tenant, Object: object, Offset: offset, Length: length,
	})
}

// SizeOf asks how large an object is, which an NFS server in front of this has
// to answer before a client will read anything.
func (c *Client) SizeOf(tenant, object string) (int64, error) {
	body, err := c.exchangeOne(request{Op: opStat, Tenant: tenant, Object: object})
	if err != nil {
		return 0, err
	}
	if len(body) != 8 {
		// A size that is not eight bytes is a far side that does not speak
		// this, and inventing one from a short answer is how a file gets
		// truncated.
		return 0, c.fail(fmt.Errorf("dataserver: a size came back as %d bytes", len(body)))
	}
	return int64(binary.BigEndian.Uint64(body)), nil
}

// exchangeOne sends one request and reads its reply.
func (c *Client) exchangeOne(req request) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return nil, c.broken
	}

	// The deadline covers the write and the read together, because they are
	// one exchange: a request half sent and a reply half read leave the
	// stream in the same place, which is nowhere.
	if err := c.conn.SetDeadline(time.Now().Add(c.exchange)); err != nil {
		return nil, c.fail(fmt.Errorf("dataserver: setting the exchange deadline: %w", err))
	}
	if err := req.encode(c.conn); err != nil {
		// A request written in part leaves the server reading a frame that
		// will never finish, so this conversation is over either way.
		return nil, c.fail(fmt.Errorf("dataserver: sending the read: %w", err))
	}
	status, data, err := decodeReply(c.conn, c.max)
	if err != nil {
		return nil, c.fail(fmt.Errorf("dataserver: reading the reply: %w", err))
	}
	if status != StatusOK {
		if e, ok := errStatus[status]; ok {
			return nil, e
		}
		return nil, fmt.Errorf("dataserver: %s", status)
	}
	return data, nil
}

// fail marks the conversation unusable and returns why, so the reason a
// caller sees is the first thing that went wrong rather than the confusion it
// caused afterwards.
func (c *Client) fail(err error) error {
	c.broken = fmt.Errorf("%w: %w", ErrBroken, err)
	c.conn.Close()
	return c.broken
}

// Close ends the conversation.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}
