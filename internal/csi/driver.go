package csi

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/mayur-tolexo/forebay/internal/grpcwire"
)

// Resolver turns a dataset name into the object behind it.
//
// It is the whole of what the controller needs from the control plane, and it
// is an interface so the driver does not carry a Kubernetes client into the
// node half, which runs on every node and should not hold cluster
// credentials.
type Resolver interface {
	// Resolve reports the object a dataset names and how large it is. A
	// dataset that exists but whose bytes are not there yet is not resolved:
	// mounting it would give a pod an empty directory and no reason for it.
	Resolve(ctx context.Context, namespace, name string) (object string, bytes int64, err error)
}

// Mounter puts a dataset under a path, and takes it away again.
//
// The node half talks to the filesystem through this so the mount logic can be
// exercised without one. Everything it does is on the node it runs on.
type Mounter interface {
	// Mount attaches source at target, read-only. It is expected to be
	// idempotent: the orchestrator repeats a publish it did not hear the
	// answer to, and a second mount over the first would stack.
	Mount(ctx context.Context, source, target string, flags []string) error
	// Unmount detaches it, and reports no error for a target that is already
	// not mounted, for the same reason.
	Unmount(ctx context.Context, target string) error
}

// Observer is told that a volume was asked for on this node.
//
// RFC-0014 is explicit that this is one direction: the driver reports and
// never asks. A mount that waited on a local storage daemon would fail a pod
// for a reason the pod has nothing to do with, so nothing here consults the
// agent or waits for it.
type Observer interface {
	// Published says a dataset of this size is now mounted on the node, and
	// Unpublished says it is gone. What is currently resident is the
	// observer's own accounting: it is asked about it on a schedule the
	// driver knows nothing about, and a mount request is an event.
	Published(volumeID string, bytes int64)
	Unpublished(volumeID string)
}

// Config is what a driver needs to serve.
type Config struct {
	// NodeID is what this node is called, which the orchestrator uses to
	// decide where a volume can be attached. Required by the node half.
	NodeID string
	// Version is reported to the orchestrator and appears in logs.
	Version string
	// Access is the address of the access layer, as an NFS server. The node
	// half mounts a dataset from here.
	Access string
	// Export is the path the access layer publishes, under which a dataset's
	// object is a directory.
	Export string
	// MountFlags are added to every mount. Read-only is not among them: it is
	// not an option this driver takes, it is applied regardless.
	MountFlags []string

	Resolver Resolver
	Mounter  Mounter
	Observer Observer
}

// Driver serves the CSI methods.
//
// It keeps nothing: every request carries what answering it needs, and an
// orchestrator repeats a request it did not hear the answer to. State here
// would be a second account of what is mounted, and the first one is the
// node's own mount table.
type Driver struct{ cfg Config }

// New builds a driver.
func New(cfg Config) (*Driver, error) {
	switch {
	case cfg.NodeID == "" && cfg.Mounter != nil:
		return nil, fmt.Errorf("csi: a node plugin needs the name of its node")
	case cfg.Mounter != nil && cfg.Access == "":
		return nil, fmt.Errorf("csi: a node plugin needs the address of the access layer")
	case cfg.Mounter != nil && cfg.Export == "":
		return nil, fmt.Errorf("csi: a node plugin needs the export the access layer publishes")
	}
	return &Driver{cfg: cfg}, nil
}

// Register wires the driver's methods onto a server.
//
// The halves are registered separately because they are deployed separately:
// the controller runs once for the cluster and the node plugin runs on every
// node, and a node that served CreateVolume would be a second answer to a
// question the cluster asks once.
func (d *Driver) Register(s *grpcwire.Server) {
	s.Register(MethodGetPluginInfo, d.getPluginInfo)
	s.Register(MethodGetPluginCapabilities, d.getPluginCapabilities)
	s.Register(MethodProbe, d.probe)

	if d.cfg.Resolver != nil {
		s.Register(MethodControllerGetCapabilities, d.controllerGetCapabilities)
		s.Register(MethodCreateVolume, d.createVolume)
		s.Register(MethodDeleteVolume, d.deleteVolume)
		s.Register(MethodValidateVolumeCapabilities, d.validate)
	}
	if d.cfg.Mounter != nil {
		s.Register(MethodNodeGetInfo, d.nodeGetInfo)
		s.Register(MethodNodeGetCapabilities, d.nodeGetCapabilities)
		s.Register(MethodNodePublishVolume, d.nodePublish)
		s.Register(MethodNodeUnpublishVolume, d.nodeUnpublish)
	}
}

func (d *Driver) getPluginInfo(context.Context, []byte) ([]byte, error) {
	return EncodePluginInfo(Name, d.cfg.Version), nil
}

func (d *Driver) getPluginCapabilities(context.Context, []byte) ([]byte, error) {
	return EncodePluginCapabilities(), nil
}

// probe reports readiness.
//
// It answers true whenever the process is up. There is nothing to check: the
// controller's dependency is the API server, which it will report on when
// asked to do something, and the node's is the access layer, which is not
// contacted until a mount. Probing them here would turn a readiness check
// into traffic against something the answer does not depend on.
func (d *Driver) probe(context.Context, []byte) ([]byte, error) {
	return EncodeProbe(true), nil
}

func (d *Driver) controllerGetCapabilities(context.Context, []byte) ([]byte, error) {
	return EncodeControllerCapabilities(), nil
}

// createVolume resolves a dataset. It allocates nothing.
//
// The name the orchestrator generates is not the dataset: it invents one per
// claim. The dataset is named in the storage class's parameters, which is
// where an administrator says which data a class hands out.
func (d *Driver) createVolume(ctx context.Context, req []byte) ([]byte, error) {
	r, err := DecodeCreateVolume(req)
	if err != nil {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "unreadable request: %v", err)
	}
	if r.Name == "" {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a volume needs a name")
	}
	if len(r.Capabilities) == 0 {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a volume needs at least one capability")
	}
	if err := d.acceptable(r.Capabilities); err != nil {
		return nil, err
	}

	namespace, name, err := datasetFrom(r.Parameters)
	if err != nil {
		return nil, err
	}
	object, bytes, err := d.cfg.Resolver.Resolve(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	return EncodeCreateVolume(Volume{
		ID:    namespace + "/" + name,
		Bytes: bytes,
		// The object travels to the node in the volume context, so the node
		// half never reads the API server: it is given what it needs at
		// publish time, and a node holding cluster credentials to look up a
		// dataset would be a much larger thing to trust.
		Context: map[string]string{
			"object":  object,
			"dataset": namespace + "/" + name,
			// Carried so the node can say how much landed without asking
			// anyone. It is the size at resolution rather than at mount, and
			// a published version does not change size.
			"bytes": strconv.FormatInt(bytes, 10),
		},
	}), nil
}

// deleteVolume forgets a resolution.
//
// Nothing was allocated, so nothing is freed. It exists because the capability
// that covers CreateVolume covers this too, and an orchestrator calls it when
// a claim goes away. Deleting the dataset here would destroy a user's data on
// the release of a claim that only ever borrowed a view of it.
func (d *Driver) deleteVolume(ctx context.Context, req []byte) ([]byte, error) {
	r, err := DecodeDeleteVolume(req)
	if err != nil {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "unreadable request: %v", err)
	}
	if r.VolumeID == "" {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a delete needs a volume id")
	}
	return nil, nil
}

// validate answers whether a volume can be used the way a caller intends.
func (d *Driver) validate(ctx context.Context, req []byte) ([]byte, error) {
	r, err := DecodeValidate(req)
	if err != nil {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "unreadable request: %v", err)
	}
	if r.VolumeID == "" {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a validation needs a volume id")
	}
	if len(r.Capabilities) == 0 {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a validation needs at least one capability")
	}
	if err := d.acceptable(r.Capabilities); err != nil {
		// Not an error: the question was answerable and the answer is no.
		// Returning a failure here would tell a caller the driver is broken
		// rather than that the volume does not do what it asked.
		return EncodeValidate(false, nil, err.Error()), nil
	}
	return EncodeValidate(true, r.Capabilities, ""), nil
}

// acceptable refuses a capability this driver cannot honour.
//
// Read-only and shared, because a published version is immutable. A writer
// admitted here would find a read-only mount at the other end, which is a
// failure a pod meets at runtime rather than at admission.
func (d *Driver) acceptable(caps []VolumeCapability) error {
	for _, c := range caps {
		if c.Block {
			return grpcwire.Errorf(grpcwire.InvalidArgument,
				"this driver serves a filesystem and was asked for %s", describe(c))
		}
		if c.AccessMode != accessMultiNodeReaderOnly {
			return grpcwire.Errorf(grpcwire.InvalidArgument,
				"a dataset is immutable and is served read-only to many readers, and this asked for %s", describe(c))
		}
	}
	return nil
}

// datasetFrom reads which dataset a storage class hands out.
func datasetFrom(params map[string]string) (namespace, name string, err error) {
	v := strings.TrimSpace(params["dataset"])
	if v == "" {
		return "", "", grpcwire.Errorf(grpcwire.InvalidArgument,
			"the storage class does not say which dataset it hands out, so set its dataset parameter to namespace/name")
	}
	ns, n, found := strings.Cut(v, "/")
	if !found || ns == "" || n == "" {
		return "", "", grpcwire.Errorf(grpcwire.InvalidArgument,
			"a dataset is named namespace/name, got %q", v)
	}
	return ns, n, nil
}

func (d *Driver) nodeGetInfo(context.Context, []byte) ([]byte, error) {
	// No limit is published. A mount costs a mount, not a device or a slot on
	// a controller, so there is no number here that would be true.
	return EncodeNodeInfo(d.cfg.NodeID, 0), nil
}

func (d *Driver) nodeGetCapabilities(context.Context, []byte) ([]byte, error) {
	return EncodeNodeCapabilities(), nil
}

// nodePublish mounts a dataset for one pod.
func (d *Driver) nodePublish(ctx context.Context, req []byte) ([]byte, error) {
	r, err := DecodeNodePublish(req)
	if err != nil {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "unreadable request: %v", err)
	}
	switch {
	case r.VolumeID == "":
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a publish needs a volume id")
	case r.TargetPath == "":
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a publish needs a target path")
	case r.Capability == nil:
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "a publish needs a volume capability")
	}
	if err := d.acceptable([]VolumeCapability{*r.Capability}); err != nil {
		return nil, err
	}

	object := r.Context["object"]
	if object == "" {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument,
			"the volume context does not say which object backs %s, which the controller sets when it resolves the dataset", r.VolumeID)
	}
	source, err := d.source(object)
	if err != nil {
		return nil, err
	}

	if err := d.cfg.Mounter.Mount(ctx, source, r.TargetPath, d.cfg.MountFlags); err != nil {
		return nil, grpcwire.Errorf(grpcwire.Internal, "mounting %s at %s: %v", source, r.TargetPath, err)
	}

	// Reported after the mount, and never before: this is an observation that
	// a dataset is resident on the node, and saying so before it is true
	// would have the agent reclaiming against a mount that then failed.
	d.observe(r.VolumeID, r.Context)
	return nil, nil
}

// source builds the address the node mounts from.
//
// The object is a key in the store, which names levels with slashes and starts
// at the top of the export. Anything else is refused rather than reinterpreted:
// the object arrives in a message, and a node plugin that cleaned a path until
// it looked reasonable would mount whatever a caller holding the socket named.
// An absolute-looking key quietly becoming a relative one is the same defect
// wearing a hat, because it mounts a different dataset than the one named.
//
// The rule is the store's own, so a key that cannot be read from a backend
// cannot be mounted from one either.
func (d *Driver) source(object string) (string, error) {
	refuse := func() (string, error) {
		return "", grpcwire.Errorf(grpcwire.InvalidArgument,
			"the object %q does not name a dataset inside the export", object)
	}
	if object == "" {
		return refuse()
	}
	for _, segment := range strings.Split(object, "/") {
		// An empty segment is a leading, trailing or doubled slash, and each
		// of those spells a level two ways.
		if segment == "" || segment == "." || segment == ".." {
			return refuse()
		}
	}
	return d.cfg.Access + ":" + path.Join("/", d.cfg.Export, object), nil
}

// nodeUnpublish takes it away.
func (d *Driver) nodeUnpublish(ctx context.Context, req []byte) ([]byte, error) {
	r, err := DecodeNodeUnpublish(req)
	if err != nil {
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "unreadable request: %v", err)
	}
	switch {
	case r.VolumeID == "":
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "an unpublish needs a volume id")
	case r.TargetPath == "":
		return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "an unpublish needs a target path")
	}
	if err := d.cfg.Mounter.Unmount(ctx, r.TargetPath); err != nil {
		return nil, grpcwire.Errorf(grpcwire.Internal, "unmounting %s: %v", r.TargetPath, err)
	}
	d.forget(r.VolumeID)
	return nil, nil
}

// observe tells the agent a dataset landed, if anything is listening.
//
// A size that cannot be read is reported as zero rather than refused. The
// mount has already happened by this point, and failing a pod because an
// observation was malformed would be the driver waiting on the agent, which
// RFC-0014 rules out in the other direction too.
func (d *Driver) observe(volumeID string, ctx map[string]string) {
	if d.cfg.Observer == nil {
		return
	}
	bytes, _ := strconv.ParseInt(ctx["bytes"], 10, 64)
	if bytes < 0 {
		bytes = 0
	}
	d.cfg.Observer.Published(volumeID, bytes)
}

// forget is the other half.
func (d *Driver) forget(volumeID string) {
	if d.cfg.Observer == nil {
		return
	}
	d.cfg.Observer.Unpublished(volumeID)
}
