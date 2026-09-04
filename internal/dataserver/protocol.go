package dataserver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The wire between the node agent and whatever serves NFS in front of it.
//
// RFC-0008 puts the data server on the node agent, and the thing a pNFS client
// talks to is an NFS server, so something has to carry a read across that
// boundary. It is deliberately small: one request, one reply, fixed header,
// no negotiation. A second reader of this protocol is a C FSAL, and every
// feature here is one somebody has to implement twice.
const (
	// magic marks a frame, so a client pointed at the wrong socket is told
	// so rather than reading a length out of somebody else's protocol.
	magic = 0x46425259 // "FBRY"
	// version is bumped when a frame's meaning changes. A reader that does
	// not know a version refuses it rather than guessing at the layout.
	//
	// Two because a request carries a third name: a listing pages on the
	// last name it saw, and a prefix and a name are two strings. Adding an
	// operation did not need this and adding a field does, which is the
	// difference the rule is about.
	version = 2

	opReadRange = 1
	// opStat asks how large an object is. An NFS server has to answer
	// getattrs before a client will read anything, and it cannot invent a
	// size: a wrong one is a truncated file or a read past the end.
	//
	// Added without bumping the version, because it changes no existing
	// frame's meaning and a reader that does not know an operation refuses it
	// by name rather than misreading it as one it does know.
	opStat = 2
	// opList asks what names are under a prefix, which is what a directory
	// is when the store has none. An NFS server cannot answer readdir
	// without it, and inventing entries would be worse than showing none.
	opList = 3
)

// Status is what the far side made of a request.
//
// The split matters more than it looks: a read past the end of an object and
// a backend that could not answer need different NFS statuses, and a caller
// that cannot tell them apart has to pick one and be wrong half the time.
type Status uint8

const (
	// StatusOK carries bytes.
	StatusOK Status = 0
	// StatusRange means the read asked for bytes the object does not have.
	StatusRange Status = 1
	// StatusRefused means the request was malformed or not allowed.
	StatusRefused Status = 2
	// StatusFailed means the read could not be answered this time. It says
	// nothing about whether it would succeed later.
	StatusFailed Status = 3
)

// String names a status for errors and logs.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusRange:
		return "out of range"
	case StatusRefused:
		return "refused"
	case StatusFailed:
		return "failed"
	default:
		return fmt.Sprintf("status(%d)", uint8(s))
	}
}

// maxName bounds a tenant or object name on the wire.
//
// The frame says how long a name is and the reader allocates that much, so
// without a bound the length field is an allocation request from whoever
// opened the socket.
const maxName = 1024

// request is one question about an object: a read, or how large it is.
type request struct {
	// Op is which question. Offset and Length are read by opReadRange, and
	// opList reads Length as how many names it may have; opStat reads
	// neither. One frame shape rather than three, because each is one thing
	// to get right in every implementation of this.
	Op     byte
	Tenant string
	// Object is the object for a read or a stat, and the prefix for a list.
	Object string
	// After is where a listing resumes, and is empty for everything else.
	// Carried in the frame rather than folded into Object, since a prefix
	// and a name are two strings and joining them would make a caller
	// unpick them.
	After  string
	Offset int64
	Length int64
}

// requestHeader is the fixed part: magic, version, op, two name lengths, the
// offset and the length.
const requestHeader = 4 + 1 + 1 + 2 + 2 + 2 + 8 + 8

// encode writes a request frame.
func (r request) encode(w io.Writer) error {
	if len(r.Tenant) > maxName || len(r.Object) > maxName || len(r.After) > maxName {
		return fmt.Errorf("dataserver: a name longer than %d bytes is not one", maxName)
	}
	buf := make([]byte, requestHeader, requestHeader+len(r.Tenant)+len(r.Object)+len(r.After))
	binary.BigEndian.PutUint32(buf[0:], magic)
	buf[4] = version
	op := r.Op
	if op == 0 {
		// A zero-valued request is a read, which is what every caller before
		// stat existed meant by one.
		op = opReadRange
	}
	buf[5] = op
	binary.BigEndian.PutUint16(buf[6:], uint16(len(r.Tenant)))
	binary.BigEndian.PutUint16(buf[8:], uint16(len(r.Object)))
	binary.BigEndian.PutUint16(buf[10:], uint16(len(r.After)))
	binary.BigEndian.PutUint64(buf[12:], uint64(r.Offset))
	binary.BigEndian.PutUint64(buf[20:], uint64(r.Length))
	buf = append(buf, r.Tenant...)
	buf = append(buf, r.Object...)
	buf = append(buf, r.After...)
	_, err := w.Write(buf)
	return err
}

// decodeRequest reads a request frame.
func decodeRequest(r io.Reader) (request, error) {
	var head [requestHeader]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return request{}, err
	}
	if got := binary.BigEndian.Uint32(head[0:]); got != magic {
		return request{}, fmt.Errorf("dataserver: %#08x is not this protocol", got)
	}
	if got := head[4]; got != version {
		return request{}, fmt.Errorf("dataserver: protocol version %d is not %d", got, version)
	}
	op := head[5]
	if op != opReadRange && op != opStat && op != opList {
		return request{}, fmt.Errorf("dataserver: operation %d is not one this speaks", op)
	}
	tenantLen := int(binary.BigEndian.Uint16(head[6:]))
	objectLen := int(binary.BigEndian.Uint16(head[8:]))
	afterLen := int(binary.BigEndian.Uint16(head[10:]))
	if tenantLen > maxName || objectLen > maxName || afterLen > maxName {
		return request{}, fmt.Errorf("dataserver: a name longer than %d bytes is not one", maxName)
	}
	names := make([]byte, tenantLen+objectLen+afterLen)
	if _, err := io.ReadFull(r, names); err != nil {
		return request{}, err
	}
	return request{
		Op:     op,
		Tenant: string(names[:tenantLen]),
		Object: string(names[tenantLen : tenantLen+objectLen]),
		After:  string(names[tenantLen+objectLen:]),
		Offset: int64(binary.BigEndian.Uint64(head[12:])),
		Length: int64(binary.BigEndian.Uint64(head[20:])),
	}, nil
}

// encodeEntries renders a listing as the reply's bytes.
//
// One length-prefixed record per name, so the C side reads it with the same
// loop it reads anything else: a two-byte name length, a flag, and a size.
// Not a header field, for the same reason a size is not: the frame every
// implementation reads carries nothing that one question needs.
func encodeEntries(entries []Entry) ([]byte, error) {
	out := make([]byte, 0, len(entries)*32)
	for _, e := range entries {
		if len(e.Name) > maxName {
			return nil, fmt.Errorf("dataserver: a name longer than %d bytes is not one", maxName)
		}
		var head [11]byte
		binary.BigEndian.PutUint16(head[0:], uint16(len(e.Name)))
		if e.Dir {
			head[2] = 1
		}
		binary.BigEndian.PutUint64(head[3:], uint64(e.Bytes))
		out = append(out, head[:]...)
		out = append(out, e.Name...)
	}
	return out, nil
}

// decodeEntries reads what encodeEntries wrote.
func decodeEntries(body []byte) ([]Entry, error) {
	var out []Entry
	for len(body) > 0 {
		if len(body) < 11 {
			return nil, fmt.Errorf("dataserver: a listing ended inside a record")
		}
		n := int(binary.BigEndian.Uint16(body[0:]))
		dir := body[2] == 1
		size := int64(binary.BigEndian.Uint64(body[3:]))
		body = body[11:]
		if len(body) < n {
			return nil, fmt.Errorf("dataserver: a listing ended inside a name")
		}
		out = append(out, Entry{Name: string(body[:n]), Dir: dir, Bytes: size})
		body = body[n:]
	}
	return out, nil
}

// replyHeader is the fixed part of a reply: magic, version, status, length.
const replyHeader = 4 + 1 + 1 + 8

// encodeReply writes a reply frame. Bytes are only carried by StatusOK.
func encodeReply(w io.Writer, status Status, data []byte) error {
	head := make([]byte, replyHeader)
	binary.BigEndian.PutUint32(head[0:], magic)
	head[4] = version
	head[5] = byte(status)
	if status != StatusOK {
		data = nil
	}
	binary.BigEndian.PutUint64(head[6:], uint64(len(data)))
	if _, err := w.Write(head); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// decodeReply reads a reply frame, refusing a payload larger than max.
//
// The bound is the caller's, not the frame's: a length field is a number
// somebody else chose, and sizing an allocation from it is how a reply
// becomes a denial of service.
func decodeReply(r io.Reader, max int64) (Status, []byte, error) {
	var head [replyHeader]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	if got := binary.BigEndian.Uint32(head[0:]); got != magic {
		return 0, nil, fmt.Errorf("dataserver: %#08x is not this protocol", got)
	}
	if got := head[4]; got != version {
		return 0, nil, fmt.Errorf("dataserver: protocol version %d is not %d", got, version)
	}
	status := Status(head[5])
	length := int64(binary.BigEndian.Uint64(head[6:]))
	switch {
	case length < 0 || length > max:
		return 0, nil, fmt.Errorf("dataserver: a reply of %d bytes is more than the %d asked for", length, max)
	case length == 0:
		return status, nil, nil
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return status, data, nil
}

// errStatus is what a status means to a Go caller.
var errStatus = map[Status]error{
	StatusRange:   errors.New("dataserver: out of range"),
	StatusRefused: errors.New("dataserver: refused"),
	StatusFailed:  errors.New("dataserver: the read could not be answered"),
}
