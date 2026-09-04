package dataserver

// Version is the protocol version, for tests that build a frame by hand.
//
// Exported to the test package rather than to callers: a frame is written by
// this package and read by the C client, and a third writer would be a third
// implementation. What the tests need is to not hardcode the number, since a
// test carrying its own copy stops testing the protocol the moment it moves.
const Version = version

// RequestHeader is the fixed part of a request frame, for the same reason: a
// test that carried its own copy of the size stopped exercising the protocol
// the moment a field was added to it, and waited for bytes that were never
// coming instead of failing.
const RequestHeader = requestHeader

// EncodeEntries and DecodeEntries are the listing's own encoding, exposed to
// the tests because the C client is its second reader: a body that ends inside
// a record is a far side that does not speak this, and answering it as an
// empty directory would show a client a dataset with nothing in it.
func EncodeEntries(entries []Entry) ([]byte, error) { return encodeEntries(entries) }
func DecodeEntries(body []byte) ([]Entry, error)    { return decodeEntries(body) }
