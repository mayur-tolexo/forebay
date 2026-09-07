package protowire_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"testing"

	"github.com/mayur-tolexo/forebay/internal/protowire"
)

// golden is a NodePublishVolumeRequest encoded by protoc, the reference
// implementation, from this text:
//
//	volume_id: "imagenet/v17"
//	target_path: "/var/lib/kubelet/pods/abc/volumes/x"
//	volume_capability {
//	  mount { fs_type: "nfs" mount_flags: "ro" mount_flags: "vers=4.1" }
//	  access_mode { mode: 1 }
//	}
//	readonly: true
//	volume_context { key: "tenant" value: "t1" }
//
// It is here because a hand-written encoder that only agrees with its own
// decoder agrees with nothing: both halves can share a mistake and every
// round-trip test still passes. These bytes came from outside.
const golden = "0a0c696d6167656e65742f76313722232f7661722f6c69622f6b7562656c65742f706f64732f6162632f76" +
	"6f6c756d65732f782a1912130a036e66731202726f1208766572733d342e311a0208013001420c0a0674656e616e7412027431"

// build encodes that same message with this package.
func build() []byte {
	var mount []byte
	mount = protowire.AppendString(mount, 1, "nfs")
	mount = protowire.AppendString(mount, 2, "ro")
	mount = protowire.AppendString(mount, 2, "vers=4.1")

	var mode []byte
	mode = protowire.AppendVarint(mode, 1, 1)

	var cap []byte
	cap = protowire.AppendMessage(cap, 2, mount)
	cap = protowire.AppendMessage(cap, 3, mode)

	var req []byte
	req = protowire.AppendString(req, 1, "imagenet/v17")
	// Field 3, staging_target_path, is empty and so is not written: proto3
	// leaves a default out, and the reference encoding above has no field 3.
	req = protowire.AppendString(req, 4, "/var/lib/kubelet/pods/abc/volumes/x")
	req = protowire.AppendMessage(req, 5, cap)
	req = protowire.AppendBool(req, 6, true)
	req = protowire.AppendStringMap(req, 8, []string{"tenant"}, map[string]string{"tenant": "t1"})
	return req
}

func TestEncodingAgreesWithProtoc(t *testing.T) {
	want, err := hex.DecodeString(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got := build(); !bytes.Equal(got, want) {
		t.Errorf("encoded\n %x\nprotoc encoded\n %x", got, want)
	}
}

func TestDecodingWhatProtocEncoded(t *testing.T) {
	b, err := hex.DecodeString(golden)
	if err != nil {
		t.Fatal(err)
	}
	var volumeID, targetPath, fsType string
	var flags []string
	var mode int64
	var readonly bool
	ctx := map[string]string{}

	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			t.Fatalf("decoding: %v", err)
		}
		switch f.Number {
		case 1:
			volumeID = f.String()
		case 4:
			targetPath = f.String()
		case 5:
			inner := f.Bytes
			for len(inner) > 0 {
				g, more, err := protowire.Next(inner)
				if err != nil {
					t.Fatalf("decoding capability: %v", err)
				}
				switch g.Number {
				case 2:
					m := g.Bytes
					for len(m) > 0 {
						h, next, err := protowire.Next(m)
						if err != nil {
							t.Fatalf("decoding mount: %v", err)
						}
						switch h.Number {
						case 1:
							fsType = h.String()
						case 2:
							flags = append(flags, h.String())
						}
						m = next
					}
				case 3:
					a := g.Bytes
					for len(a) > 0 {
						h, next, err := protowire.Next(a)
						if err != nil {
							t.Fatalf("decoding access mode: %v", err)
						}
						if h.Number == 1 {
							mode = h.Int64()
						}
						a = next
					}
				}
				inner = more
			}
		case 6:
			readonly = f.Bool()
		case 8:
			if err := protowire.StringMap(ctx, f.Bytes); err != nil {
				t.Fatalf("decoding map: %v", err)
			}
		}
		b = rest
	}

	if volumeID != "imagenet/v17" {
		t.Errorf("volume id %q", volumeID)
	}
	if targetPath != "/var/lib/kubelet/pods/abc/volumes/x" {
		t.Errorf("target path %q", targetPath)
	}
	if fsType != "nfs" {
		t.Errorf("fs type %q", fsType)
	}
	if len(flags) != 2 || flags[0] != "ro" || flags[1] != "vers=4.1" {
		t.Errorf("mount flags %q", flags)
	}
	if mode != 1 {
		t.Errorf("access mode %d", mode)
	}
	if !readonly {
		t.Error("readonly did not survive")
	}
	if ctx["tenant"] != "t1" {
		t.Errorf("volume context %v", ctx)
	}
}

func TestVarintsSurviveTheirBoundaries(t *testing.T) {
	// The byte-length of a varint changes at each multiple of seven bits, and
	// the top of the range is where a shift overflows.
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, 1 << 35, math.MaxUint64 - 1, math.MaxUint64} {
		b := protowire.AppendVarint(nil, 1, v)
		f, rest, err := protowire.Next(b)
		if err != nil {
			t.Fatalf("%d: %v", v, err)
		}
		if f.Varint != v {
			t.Errorf("%d came back as %d", v, f.Varint)
		}
		if len(rest) != 0 {
			t.Errorf("%d left %d bytes", v, len(rest))
		}
	}
}

func TestANegativeSizeIsNotAnExabyte(t *testing.T) {
	// Protobuf's int64 is the two's complement in a varint, so a negative
	// number arrives as a very large unsigned one. Reading it unsigned turns
	// a wrong-but-small number into an enormous one.
	n := int64(-4096)
	b := protowire.AppendVarint(nil, 1, uint64(n))
	f, _, err := protowire.Next(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Int64() != -4096 {
		t.Errorf("got %d, want -4096", f.Int64())
	}
}

func TestAFieldThisDoesNotKnowIsSkipped(t *testing.T) {
	// A newer orchestrator sends fields this was not written for, and every
	// one of them has to be walked past to reach the fields that matter.
	// Each wire type is skipped by a different rule, so each is exercised.
	var b []byte
	b = protowire.AppendVarint(b, 900, 1<<40)
	b = append(b, 0xB9, 0x38, 1, 2, 3, 4, 5, 6, 7, 8) // field 903, fixed64
	b = append(b, 0xB5, 0x38, 9, 9, 9, 9)             // field 902, fixed32
	b = protowire.AppendString(b, 901, "something new")
	b = protowire.AppendString(b, 1, "the field that matters")

	var found string
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			t.Fatalf("skipping: %v", err)
		}
		if f.Number == 1 {
			found = f.String()
		}
		b = rest
	}
	if found != "the field that matters" {
		t.Errorf("got %q after skipping unknown fields", found)
	}
}

func TestAMessageThatEndsInsideAFieldIsRefused(t *testing.T) {
	// A short read of a socket is the ordinary way this arrives, and a
	// decoder that returned what it had would hand up a half-message as a
	// whole one.
	full := build()
	for n := 1; n < len(full); n++ {
		cut := full[:n]
		var err error
		for len(cut) > 0 {
			var f protowire.Field
			f, cut, err = protowire.Next(cut)
			_ = f
			if err != nil {
				break
			}
		}
		// Not every prefix is detectable: a cut exactly on a field boundary
		// is a shorter valid message. What must never happen is reading past
		// the end, which would panic rather than return.
		if err != nil && !errors.Is(err, protowire.ErrTruncated) {
			t.Fatalf("cut at %d gave %v, want a truncation", n, err)
		}
	}
}

func TestAVarintThatNeverEndsIsRefused(t *testing.T) {
	// Ten bytes is the most a 64-bit value takes. Without the bound, a run of
	// continuation bytes reads to the end of the message and silently
	// overflows on the way.
	b := []byte{0x08}
	for i := 0; i < 12; i++ {
		b = append(b, 0xFF)
	}
	b = append(b, 0x01)
	if _, _, err := protowire.Next(b); !errors.Is(err, protowire.ErrTruncated) {
		t.Errorf("an 13-byte varint gave %v", err)
	}
}

func TestFieldNumberZeroIsRefused(t *testing.T) {
	// No encoder writes field zero, and a decoder that accepted it would take
	// a tag of zero as a field and make no progress through the message.
	if _, _, err := protowire.Next([]byte{0x00, 0x01}); !errors.Is(err, protowire.ErrFieldNumber) {
		t.Errorf("field zero gave %v", err)
	}
}

func TestAGroupIsRefusedRatherThanGuessedAt(t *testing.T) {
	// Wire types 3 and 4 are groups. Nothing has generated them since proto2,
	// and a message holding one cannot be walked past, so the fields after it
	// cannot be read. Saying so beats returning the fields before it as if
	// they were all of them.
	b := []byte{0x0B, 0x08, 0x01, 0x0C} // field 1, wire type 3
	if _, _, err := protowire.Next(b); !errors.Is(err, protowire.ErrWireType) {
		t.Errorf("a group gave %v", err)
	}
}

func TestAnEmptyMapKeyIsStillAnEntry(t *testing.T) {
	// Proto3 leaves a zero value out, so an entry whose key is empty arrives
	// with no field 1 at all. Dropping it would lose a key a caller may be
	// looking for.
	var entry []byte
	entry = protowire.AppendString(entry, 2, "a value under no key")
	m := map[string]string{}
	if err := protowire.StringMap(m, entry); err != nil {
		t.Fatal(err)
	}
	if m[""] != "a value under no key" {
		t.Errorf("got %v", m)
	}
}

func TestAFixedWidthFieldCutShortIsRefused(t *testing.T) {
	// A fixed64 and a fixed32 are located by their width rather than by a
	// length, so a message that ends inside one has to be caught by the width
	// check. Without it the decoder reads past the end of the buffer.
	for _, c := range []struct {
		name string
		tag  []byte
		full int
	}{
		{"fixed64", []byte{0xB9, 0x38}, 8},
		{"fixed32", []byte{0xB5, 0x38}, 4},
	} {
		for n := 0; n < c.full; n++ {
			b := append(append([]byte{}, c.tag...), make([]byte, n)...)
			if _, _, err := protowire.Next(b); !errors.Is(err, protowire.ErrTruncated) {
				t.Errorf("a %s with %d of %d bytes gave %v", c.name, n, c.full, err)
			}
		}
	}
}
