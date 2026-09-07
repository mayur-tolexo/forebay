// Package protowire encodes and decodes the parts of protocol buffers that
// CSI uses.
//
// CSI is defined as a gRPC service, and gRPC carries protobuf. This project
// takes no dependencies, so the encoding is here. It is deliberately partial:
// CSI's messages are strings, integers, booleans, maps of string to string,
// and nesting of those, so those are what this handles. There is no schema, no
// reflection and no generated code, because a hand-written message is read
// once by a person and a generated one is never read at all.
//
// Unknown fields are skipped rather than refused. A protobuf reader is
// required to, and CSI's own compatibility story depends on it: a newer
// orchestrator sends fields this does not know about, and refusing them would
// break against every version but the one this was written for.
package protowire

import (
	"errors"
	"fmt"
)

// Wire types, which are the low three bits of a field's tag.
const (
	// WireVarint carries integers and booleans.
	WireVarint = 0
	// WireFixed64 and WireFixed32 carry fixed-width numbers. CSI has none,
	// but a message from a newer peer may, and skipping a field needs to
	// know how long it is.
	WireFixed64 = 1
	// WireBytes carries strings, byte strings and nested messages, which is
	// most of CSI.
	WireBytes   = 2
	WireFixed32 = 5
)

var (
	// ErrTruncated reports a message that ended inside a field.
	ErrTruncated = errors.New("protowire: the message ended inside a field")
	// ErrWireType reports a field whose wire type is not one this knows how
	// to skip, which means the rest of the message cannot be located.
	ErrWireType = errors.New("protowire: unknown wire type")
	// ErrFieldNumber reports a tag with a field number of zero, which no
	// encoder produces and which would make a decoder loop.
	ErrFieldNumber = errors.New("protowire: field number must be positive")
)

// AppendVarint writes a varint field.
func AppendVarint(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, WireVarint)
	return appendUvarint(b, v)
}

// AppendBool writes a boolean field.
//
// False is written rather than omitted. Proto3 lets an encoder leave a zero
// value out and CSI has a boolean that means something when it is false --
// NodePublishVolume's readonly -- so the choice is between writing it and
// having the reader supply the default. Writing it says what was meant.
func AppendBool(b []byte, field int, v bool) []byte {
	var n uint64
	if v {
		n = 1
	}
	return AppendVarint(b, field, n)
}

// AppendString writes a string field.
func AppendString(b []byte, field int, s string) []byte {
	b = appendTag(b, field, WireBytes)
	b = appendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// AppendMessage writes a nested message, which is a length-delimited field
// holding an already-encoded message.
func AppendMessage(b []byte, field int, msg []byte) []byte {
	b = appendTag(b, field, WireBytes)
	b = appendUvarint(b, uint64(len(msg)))
	return append(b, msg...)
}

// AppendStringMap writes a map<string, string>.
//
// A map is repeated entries, each a message of key at field 1 and value at
// field 2, which is what the wire format makes of a map declaration. Entries
// go out in the order given: protobuf maps are unordered and a reader must not
// depend on it, so nothing here sorts them into an order that would look
// meaningful.
func AppendStringMap(b []byte, field int, keys []string, m map[string]string) []byte {
	for _, k := range keys {
		var e []byte
		e = AppendString(e, 1, k)
		e = AppendString(e, 2, m[k])
		b = AppendMessage(b, field, e)
	}
	return b
}

// Field is one decoded field, still holding its raw value.
type Field struct {
	// Number is the field number from the tag.
	Number int
	// Wire is the wire type, which says which of the two values below holds
	// this field's content.
	Wire int
	// Varint holds the value of a varint, fixed32 or fixed64 field.
	Varint uint64
	// Bytes holds the content of a length-delimited field. It aliases the
	// message it was decoded from rather than copying, so a caller keeping it
	// past the life of that buffer must copy it.
	Bytes []byte
}

// String reads a length-delimited field as a string.
func (f Field) String() string { return string(f.Bytes) }

// Bool reads a varint field as a boolean, the way protobuf defines it: zero is
// false and everything else is true.
func (f Field) Bool() bool { return f.Varint != 0 }

// Int64 reads a varint field as a signed integer.
//
// Protobuf's int64 is a varint holding the two's complement, so a negative
// number arrives as a very large unsigned one and is converted back rather
// than clamped. CSI's sizes are never negative, but reading one as 18
// exabytes would be a worse answer than reading it as what it is.
func (f Field) Int64() int64 { return int64(f.Varint) }

// Next decodes the field at the front of b and returns what is left.
//
// It is the only decoder here: a message is a sequence of fields and every
// message type is read by looping over this and switching on the number.
func Next(b []byte) (Field, []byte, error) {
	var f Field
	tag, rest, err := uvarint(b)
	if err != nil {
		return f, nil, err
	}
	f.Number = int(tag >> 3)
	f.Wire = int(tag & 7)
	if f.Number <= 0 {
		return f, nil, fmt.Errorf("%w, got %d", ErrFieldNumber, f.Number)
	}

	switch f.Wire {
	case WireVarint:
		f.Varint, rest, err = uvarint(rest)
		if err != nil {
			return f, nil, err
		}
	case WireBytes:
		var n uint64
		n, rest, err = uvarint(rest)
		if err != nil {
			return f, nil, err
		}
		// Checked against what is left rather than against a limit: a
		// length longer than the message is the only way this can be
		// asked to read past the end, and the message is already in
		// memory.
		if n > uint64(len(rest)) {
			return f, nil, fmt.Errorf("%w: a field of %d bytes with %d left", ErrTruncated, n, len(rest))
		}
		f.Bytes = rest[:n]
		rest = rest[n:]
	case WireFixed64:
		if len(rest) < 8 {
			return f, nil, fmt.Errorf("%w: a fixed64 with %d bytes left", ErrTruncated, len(rest))
		}
		for i := 7; i >= 0; i-- {
			f.Varint = f.Varint<<8 | uint64(rest[i])
		}
		rest = rest[8:]
	case WireFixed32:
		if len(rest) < 4 {
			return f, nil, fmt.Errorf("%w: a fixed32 with %d bytes left", ErrTruncated, len(rest))
		}
		for i := 3; i >= 0; i-- {
			f.Varint = f.Varint<<8 | uint64(rest[i])
		}
		rest = rest[4:]
	default:
		// Groups, which are the two wire types not handled above. They
		// were removed from the language before proto3 and nothing
		// generates them, and a message holding one cannot be walked
		// past without implementing them.
		return f, nil, fmt.Errorf("%w: %d in field %d", ErrWireType, f.Wire, f.Number)
	}
	return f, rest, nil
}

// StringMap decodes one map entry into m, which the caller creates.
//
// A repeated map field arrives one entry at a time, so this is called per
// entry rather than being handed the whole map.
func StringMap(m map[string]string, entry []byte) error {
	var k, v string
	for len(entry) > 0 {
		f, rest, err := Next(entry)
		if err != nil {
			return err
		}
		switch {
		case f.Number == 1 && f.Wire == WireBytes:
			k = f.String()
		case f.Number == 2 && f.Wire == WireBytes:
			v = f.String()
		}
		entry = rest
	}
	// An entry with no key is still an entry, and proto3 leaves a zero value
	// out: a map with an empty key is what was sent, and dropping it here
	// would silently lose a key a caller may be looking for.
	m[k] = v
	return nil
}

// appendTag writes a field's number and wire type.
func appendTag(b []byte, field, wire int) []byte {
	return appendUvarint(b, uint64(field)<<3|uint64(wire))
}

// appendUvarint writes base-128 with the continuation bit set on every byte
// but the last.
func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// uvarint reads one, refusing an encoding that would not fit in 64 bits.
//
// Ten bytes is the most a 64-bit value takes and the tenth carries a single
// bit, so anything above one in it is a sender not encoding a 64-bit number.
// That check is also what bounds the loop: a tenth byte either ends the varint
// or fails here, so there is never an eleventh.
func uvarint(b []byte) (uint64, []byte, error) {
	var v uint64
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, nil, fmt.Errorf("%w: a varint wider than 64 bits", ErrTruncated)
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, b[i+1:], nil
		}
	}
	return 0, nil, fmt.Errorf("%w: a varint ran to the end", ErrTruncated)
}
