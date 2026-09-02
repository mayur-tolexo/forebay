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
	version = 1

	opReadRange = 1
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

// request is a read of one object.
type request struct {
	Tenant string
	Object string
	Offset int64
	Length int64
}

// requestHeader is the fixed part: magic, version, op, two name lengths, the
// offset and the length.
const requestHeader = 4 + 1 + 1 + 2 + 2 + 8 + 8

// encode writes a request frame.
func (r request) encode(w io.Writer) error {
	if len(r.Tenant) > maxName || len(r.Object) > maxName {
		return fmt.Errorf("dataserver: a name longer than %d bytes is not one", maxName)
	}
	buf := make([]byte, requestHeader, requestHeader+len(r.Tenant)+len(r.Object))
	binary.BigEndian.PutUint32(buf[0:], magic)
	buf[4] = version
	buf[5] = opReadRange
	binary.BigEndian.PutUint16(buf[6:], uint16(len(r.Tenant)))
	binary.BigEndian.PutUint16(buf[8:], uint16(len(r.Object)))
	binary.BigEndian.PutUint64(buf[10:], uint64(r.Offset))
	binary.BigEndian.PutUint64(buf[18:], uint64(r.Length))
	buf = append(buf, r.Tenant...)
	buf = append(buf, r.Object...)
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
	if got := head[5]; got != opReadRange {
		return request{}, fmt.Errorf("dataserver: operation %d is not one this speaks", got)
	}
	tenantLen := int(binary.BigEndian.Uint16(head[6:]))
	objectLen := int(binary.BigEndian.Uint16(head[8:]))
	if tenantLen > maxName || objectLen > maxName {
		return request{}, fmt.Errorf("dataserver: a name longer than %d bytes is not one", maxName)
	}
	names := make([]byte, tenantLen+objectLen)
	if _, err := io.ReadFull(r, names); err != nil {
		return request{}, err
	}
	return request{
		Tenant: string(names[:tenantLen]),
		Object: string(names[tenantLen:]),
		Offset: int64(binary.BigEndian.Uint64(head[10:])),
		Length: int64(binary.BigEndian.Uint64(head[18:])),
	}, nil
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
