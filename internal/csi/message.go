// Package csi serves the Container Storage Interface for Forebay.
//
// RFC-0014 fixes what it does and, more to the point, what it does not. The
// driver attaches a dataset, which lives in a durable backend and is reached
// through the access layer. It never hands out borrowed capacity as a volume:
// a volume promises the data stays until the pod is done with it, and borrowed
// capacity is defined by not staying.
//
// So there is no provisioning here. The controller resolves a name to
// something mountable and allocates nothing, and the node mounts it read-only,
// because a published version is immutable.
package csi

import (
	"fmt"
	"sort"

	"github.com/mayur-tolexo/forebay/internal/protowire"
)

// Name is how this driver identifies itself.
//
// It matches the API group the project's own kind lives under, because a
// cluster with one of them has the other and two spellings would be two
// things to keep in step.
const Name = "csi.forebay.io"

// The methods this driver serves, spelled the way they travel.
const (
	MethodGetPluginInfo         = "/csi.v1.Identity/GetPluginInfo"
	MethodGetPluginCapabilities = "/csi.v1.Identity/GetPluginCapabilities"
	MethodProbe                 = "/csi.v1.Identity/Probe"

	MethodControllerGetCapabilities  = "/csi.v1.Controller/ControllerGetCapabilities"
	MethodCreateVolume               = "/csi.v1.Controller/CreateVolume"
	MethodDeleteVolume               = "/csi.v1.Controller/DeleteVolume"
	MethodValidateVolumeCapabilities = "/csi.v1.Controller/ValidateVolumeCapabilities"

	MethodNodeGetInfo         = "/csi.v1.Node/NodeGetInfo"
	MethodNodeGetCapabilities = "/csi.v1.Node/NodeGetCapabilities"
	MethodNodePublishVolume   = "/csi.v1.Node/NodePublishVolume"
	MethodNodeUnpublishVolume = "/csi.v1.Node/NodeUnpublishVolume"
)

// Enum values from the CSI specification. They travel as numbers and cannot be
// renumbered.
const (
	// serviceController says this driver runs a controller plugin, which is
	// what makes an orchestrator call the controller methods at all.
	serviceController = 1
	// rpcCreateDeleteVolume is the controller capability that covers
	// CreateVolume and DeleteVolume. Resolving a dataset is done under it
	// even though nothing is created, because there is no capability in the
	// specification for "resolves an existing thing".
	rpcCreateDeleteVolume = 1
	// accessMultiNodeReaderOnly is the only access mode this driver accepts.
	// A published version is immutable and every reader gets the same bytes,
	// which is exactly what this mode describes.
	accessMultiNodeReaderOnly = 3
)

// VolumeCapability is what an orchestrator says it wants of a volume.
type VolumeCapability struct {
	// Mount is set when the volume is asked for as a filesystem, and Block
	// when it is asked for as a raw device. This driver serves a filesystem,
	// so a request naming Block is refused rather than quietly mounted.
	Mount *MountCapability
	Block bool
	// AccessMode is the sharing the caller expects.
	AccessMode int64
}

// MountCapability is the filesystem half of a capability.
type MountCapability struct {
	FSType string
	Flags  []string
}

// decodeVolumeCapability reads one.
func decodeVolumeCapability(b []byte) (VolumeCapability, error) {
	var c VolumeCapability
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			return c, err
		}
		switch f.Number {
		case 1:
			c.Block = true
		case 2:
			m := &MountCapability{}
			inner := f.Bytes
			for len(inner) > 0 {
				g, more, err := protowire.Next(inner)
				if err != nil {
					return c, err
				}
				switch g.Number {
				case 1:
					m.FSType = g.String()
				case 2:
					m.Flags = append(m.Flags, g.String())
				}
				inner = more
			}
			c.Mount = m
		case 3:
			inner := f.Bytes
			for len(inner) > 0 {
				g, more, err := protowire.Next(inner)
				if err != nil {
					return c, err
				}
				if g.Number == 1 {
					c.AccessMode = g.Int64()
				}
				inner = more
			}
		}
		b = rest
	}
	return c, nil
}

// encodeVolumeCapability writes one, which a validation reply echoes back.
func encodeVolumeCapability(c VolumeCapability) []byte {
	var out []byte
	if c.Block {
		out = protowire.AppendMessage(out, 1, nil)
	}
	if c.Mount != nil {
		var m []byte
		if c.Mount.FSType != "" {
			m = protowire.AppendString(m, 1, c.Mount.FSType)
		}
		for _, fl := range c.Mount.Flags {
			m = protowire.AppendString(m, 2, fl)
		}
		out = protowire.AppendMessage(out, 2, m)
	}
	var mode []byte
	mode = protowire.AppendVarint(mode, 1, uint64(c.AccessMode))
	return protowire.AppendMessage(out, 3, mode)
}

// CreateVolumeRequest is what an orchestrator sends to have a volume resolved.
type CreateVolumeRequest struct {
	Name         string
	Capabilities []VolumeCapability
	Parameters   map[string]string
}

// DecodeCreateVolume reads one.
func DecodeCreateVolume(b []byte) (CreateVolumeRequest, error) {
	r := CreateVolumeRequest{Parameters: map[string]string{}}
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			return r, err
		}
		switch f.Number {
		case 1:
			r.Name = f.String()
		case 3:
			c, err := decodeVolumeCapability(f.Bytes)
			if err != nil {
				return r, err
			}
			r.Capabilities = append(r.Capabilities, c)
		case 4:
			if err := protowire.StringMap(r.Parameters, f.Bytes); err != nil {
				return r, err
			}
		}
		b = rest
	}
	return r, nil
}

// Volume is what a resolved dataset looks like to an orchestrator.
type Volume struct {
	ID      string
	Bytes   int64
	Context map[string]string
}

// EncodeCreateVolume writes the reply.
func EncodeCreateVolume(v Volume) []byte {
	var vol []byte
	if v.Bytes > 0 {
		vol = protowire.AppendVarint(vol, 1, uint64(v.Bytes))
	}
	vol = protowire.AppendString(vol, 2, v.ID)
	vol = protowire.AppendStringMap(vol, 3, sortedKeys(v.Context), v.Context)
	return protowire.AppendMessage(nil, 1, vol)
}

// DeleteVolumeRequest names what to forget.
type DeleteVolumeRequest struct{ VolumeID string }

// DecodeDeleteVolume reads one.
func DecodeDeleteVolume(b []byte) (DeleteVolumeRequest, error) {
	var r DeleteVolumeRequest
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			return r, err
		}
		if f.Number == 1 {
			r.VolumeID = f.String()
		}
		b = rest
	}
	return r, nil
}

// ValidateRequest asks whether a volume can be used a particular way.
type ValidateRequest struct {
	VolumeID     string
	Capabilities []VolumeCapability
	Context      map[string]string
}

// DecodeValidate reads one.
func DecodeValidate(b []byte) (ValidateRequest, error) {
	r := ValidateRequest{Context: map[string]string{}}
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			return r, err
		}
		switch f.Number {
		case 1:
			r.VolumeID = f.String()
		case 2:
			if err := protowire.StringMap(r.Context, f.Bytes); err != nil {
				return r, err
			}
		case 3:
			c, err := decodeVolumeCapability(f.Bytes)
			if err != nil {
				return r, err
			}
			r.Capabilities = append(r.Capabilities, c)
		}
		b = rest
	}
	return r, nil
}

// EncodeValidate writes the reply.
//
// A confirmation is the whole message being present: the specification says an
// empty reply with a message means the capabilities were not confirmed, and a
// caller reads presence rather than a boolean.
func EncodeValidate(confirmed bool, caps []VolumeCapability, why string) []byte {
	var out []byte
	if confirmed {
		var c []byte
		for _, vc := range caps {
			c = protowire.AppendMessage(c, 2, encodeVolumeCapability(vc))
		}
		out = protowire.AppendMessage(out, 1, c)
	}
	if why != "" {
		out = protowire.AppendString(out, 2, why)
	}
	return out
}

// NodePublishRequest is a mount request for one pod on this node.
type NodePublishRequest struct {
	VolumeID   string
	TargetPath string
	Capability *VolumeCapability
	ReadOnly   bool
	Context    map[string]string
}

// DecodeNodePublish reads one.
func DecodeNodePublish(b []byte) (NodePublishRequest, error) {
	r := NodePublishRequest{Context: map[string]string{}}
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			return r, err
		}
		switch f.Number {
		case 1:
			r.VolumeID = f.String()
		case 4:
			r.TargetPath = f.String()
		case 5:
			c, err := decodeVolumeCapability(f.Bytes)
			if err != nil {
				return r, err
			}
			r.Capability = &c
		case 6:
			r.ReadOnly = f.Bool()
		case 8:
			if err := protowire.StringMap(r.Context, f.Bytes); err != nil {
				return r, err
			}
		}
		b = rest
	}
	return r, nil
}

// NodeUnpublishRequest is the other half.
type NodeUnpublishRequest struct {
	VolumeID   string
	TargetPath string
}

// DecodeNodeUnpublish reads one.
func DecodeNodeUnpublish(b []byte) (NodeUnpublishRequest, error) {
	var r NodeUnpublishRequest
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			return r, err
		}
		switch f.Number {
		case 1:
			r.VolumeID = f.String()
		case 2:
			r.TargetPath = f.String()
		}
		b = rest
	}
	return r, nil
}

// EncodePluginInfo writes the driver's name and version.
func EncodePluginInfo(name, version string) []byte {
	var out []byte
	out = protowire.AppendString(out, 1, name)
	if version != "" {
		out = protowire.AppendString(out, 2, version)
	}
	return out
}

// EncodePluginCapabilities says which services this plugin runs.
func EncodePluginCapabilities() []byte {
	var svc []byte
	svc = protowire.AppendVarint(svc, 1, serviceController)
	var capability []byte
	capability = protowire.AppendMessage(capability, 1, svc)
	return protowire.AppendMessage(nil, 1, capability)
}

// EncodeProbe says whether the driver is ready to serve.
func EncodeProbe(ready bool) []byte {
	// The field is a BoolValue, a message wrapping the boolean, so that "not
	// answered" and "answered false" are different things on the wire.
	var wrapper []byte
	wrapper = protowire.AppendBool(wrapper, 1, ready)
	return protowire.AppendMessage(nil, 1, wrapper)
}

// EncodeControllerCapabilities says what the controller half does.
func EncodeControllerCapabilities() []byte {
	var rpc []byte
	rpc = protowire.AppendVarint(rpc, 1, rpcCreateDeleteVolume)
	var capability []byte
	capability = protowire.AppendMessage(capability, 1, rpc)
	return protowire.AppendMessage(nil, 1, capability)
}

// EncodeNodeInfo names this node.
func EncodeNodeInfo(nodeID string, maxVolumes int64) []byte {
	var out []byte
	out = protowire.AppendString(out, 1, nodeID)
	if maxVolumes > 0 {
		out = protowire.AppendVarint(out, 2, uint64(maxVolumes))
	}
	return out
}

// EncodeNodeCapabilities says what the node half does.
//
// Nothing, which is a real answer: this driver has no staging step, because
// there is nothing to attach before a mount and a second step would be a
// no-op the orchestrator waits for.
func EncodeNodeCapabilities() []byte { return nil }

// sortedKeys orders a map's keys so an encoding is the same twice.
//
// Protobuf maps are unordered and no reader may depend on it, but a reply that
// differs run to run is one an operator cannot diff, and a test cannot compare
// against a constant.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describe renders a capability for an error message, so a refusal says what
// was asked for rather than only that it was refused.
func describe(c VolumeCapability) string {
	switch {
	case c.Block:
		return "a block device"
	case c.Mount != nil && c.Mount.FSType != "":
		return fmt.Sprintf("a %s filesystem with access mode %d", c.Mount.FSType, c.AccessMode)
	default:
		return fmt.Sprintf("a filesystem with access mode %d", c.AccessMode)
	}
}
