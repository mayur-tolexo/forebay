package csi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/csi"
	"github.com/mayur-tolexo/forebay/internal/grpcwire"
	"github.com/mayur-tolexo/forebay/internal/protowire"
)

// fakeResolver answers for the datasets it was given.
type fakeResolver struct {
	objects map[string]string
	bytes   map[string]int64
	err     error
}

func (f *fakeResolver) Resolve(ctx context.Context, ns, name string) (string, int64, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	key := ns + "/" + name
	o, ok := f.objects[key]
	if !ok {
		return "", 0, grpcwire.Errorf(grpcwire.NotFound, "no dataset %s", key)
	}
	return o, f.bytes[key], nil
}

// fakeMounter records what it was asked to do.
type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]string
	flags   []string
	err     error
}

func newMounter() *fakeMounter { return &fakeMounter{mounted: map[string]string{}} }

func (m *fakeMounter) Mount(ctx context.Context, source, target string, flags []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.mounted[target] = source
	m.flags = flags
	return nil
}

func (m *fakeMounter) Unmount(ctx context.Context, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	delete(m.mounted, target)
	return nil
}

func (m *fakeMounter) sourceOf(target string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted[target]
}

// fakeObserver records what the node was told.
type fakeObserver struct {
	mu   sync.Mutex
	seen []string
}

func (o *fakeObserver) Published(id string, bytes int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, fmt.Sprintf("published %s %d", id, bytes))
}

func (o *fakeObserver) Unpublished(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, "unpublished "+id)
}

func (o *fakeObserver) record() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seen...)
}

// readOnlyMount is the capability an orchestrator sends for a dataset.
func readOnlyMount() []byte {
	var m []byte
	m = protowire.AppendString(m, 1, "nfs")
	var mode []byte
	mode = protowire.AppendVarint(mode, 1, 3) // MULTI_NODE_READER_ONLY
	var c []byte
	c = protowire.AppendMessage(c, 2, m)
	return protowire.AppendMessage(c, 3, mode)
}

func writerMount() []byte {
	var m []byte
	m = protowire.AppendString(m, 1, "nfs")
	var mode []byte
	mode = protowire.AppendVarint(mode, 1, 1) // SINGLE_NODE_WRITER
	var c []byte
	c = protowire.AppendMessage(c, 2, m)
	return protowire.AppendMessage(c, 3, mode)
}

func blockVolume() []byte {
	var c []byte
	c = protowire.AppendMessage(c, 1, nil)
	var mode []byte
	mode = protowire.AppendVarint(mode, 1, 3)
	return protowire.AppendMessage(c, 3, mode)
}

// createRequest builds a CreateVolumeRequest.
func createRequest(name, dataset string, capability []byte) []byte {
	var b []byte
	b = protowire.AppendString(b, 1, name)
	b = protowire.AppendMessage(b, 3, capability)
	if dataset != "" {
		b = protowire.AppendStringMap(b, 4, []string{"dataset"}, map[string]string{"dataset": dataset})
	}
	return b
}

// publishRequest builds a NodePublishVolumeRequest.
func publishRequest(id, target string, capability []byte, ctx map[string]string) []byte {
	var b []byte
	b = protowire.AppendString(b, 1, id)
	b = protowire.AppendString(b, 4, target)
	b = protowire.AppendMessage(b, 5, capability)
	b = protowire.AppendBool(b, 6, true)
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	// Sorted so the encoding is the same twice, which a failure message needs.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return protowire.AppendStringMap(b, 8, keys, ctx)
}

// controller builds a controller-only driver.
func controller(t *testing.T, r csi.Resolver) *csi.Driver {
	t.Helper()
	d, err := csi.New(csi.Config{Version: "test", Resolver: r})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// node builds a node-only driver.
func node(t *testing.T, m csi.Mounter, o csi.Observer) *csi.Driver {
	t.Helper()
	d, err := csi.New(csi.Config{
		NodeID: "worker-3", Version: "test",
		Access: "10.0.0.9", Export: "forebay",
		MountFlags: []string{"vers=4.1"},
		Mounter:    m, Observer: o,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// call routes one method through a real server, so the tests exercise the
// same path an orchestrator does rather than calling the handler directly.
func call(t *testing.T, d *csi.Driver, method string, req []byte) ([]byte, error) {
	t.Helper()
	s := grpcwire.NewServer()
	d.Register(s)
	return callServer(t, s, method, req)
}

func TestResolvingADatasetAllocatesNothingAndNamesTheObject(t *testing.T) {
	d := controller(t, &fakeResolver{
		objects: map[string]string{"team/imagenet": "imagenet/v17"},
		bytes:   map[string]int64{"team/imagenet": 12 << 20},
	})
	got, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc-abc", "team/imagenet", readOnlyMount()))
	if err != nil {
		t.Fatal(err)
	}

	id, bytes, ctx := decodeVolume(t, got)
	if id != "team/imagenet" {
		t.Errorf("volume id %q", id)
	}
	if bytes != 12<<20 {
		t.Errorf("size %d", bytes)
	}
	// The object has to reach the node: it is what the node mounts, and a node
	// that had to look it up would need cluster credentials.
	if ctx["object"] != "imagenet/v17" {
		t.Errorf("volume context %v", ctx)
	}
	if ctx["bytes"] != "12582912" {
		t.Errorf("size did not travel: %v", ctx)
	}
}

func TestAStorageClassThatNamesNoDatasetIsRefused(t *testing.T) {
	// The name the orchestrator generates is per claim and says nothing about
	// which data is wanted. Without the parameter there is nothing to resolve,
	// and guessing would hand a pod somebody else's dataset.
	d := controller(t, &fakeResolver{})
	_, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc-abc", "", readOnlyMount()))
	if grpcwire.CodeOf(err) != grpcwire.InvalidArgument {
		t.Fatalf("got %v from %v", grpcwire.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "dataset parameter") {
		t.Errorf("the message does not say what to set: %v", err)
	}
}

func TestAMalformedDatasetNameIsRefused(t *testing.T) {
	d := controller(t, &fakeResolver{})
	for _, name := range []string{"imagenet", "/imagenet", "team/", "/"} {
		_, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc-abc", name, readOnlyMount()))
		if grpcwire.CodeOf(err) != grpcwire.InvalidArgument {
			t.Errorf("%q gave %v", name, grpcwire.CodeOf(err))
		}
	}
}

func TestADatasetThatIsNotThereIsNotFound(t *testing.T) {
	// NotFound rather than Internal, because the provisioner retries one and
	// reports the other to the user.
	d := controller(t, &fakeResolver{objects: map[string]string{}})
	_, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc-abc", "team/missing", readOnlyMount()))
	if grpcwire.CodeOf(err) != grpcwire.NotFound {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}

func TestAWriterIsRefusedBeforeItReachesAPod(t *testing.T) {
	// A published version is immutable. A writer admitted here would meet a
	// read-only mount at runtime, which is a failure the pod cannot act on and
	// the user never sees the reason for.
	d := controller(t, &fakeResolver{objects: map[string]string{"team/imagenet": "imagenet/v17"}})
	_, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc-abc", "team/imagenet", writerMount()))
	if grpcwire.CodeOf(err) != grpcwire.InvalidArgument {
		t.Fatalf("got %v from %v", grpcwire.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Errorf("the message does not say why: %v", err)
	}
}

func TestABlockDeviceIsRefused(t *testing.T) {
	d := controller(t, &fakeResolver{objects: map[string]string{"team/imagenet": "imagenet/v17"}})
	_, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc-abc", "team/imagenet", blockVolume()))
	if grpcwire.CodeOf(err) != grpcwire.InvalidArgument {
		t.Fatalf("got %v from %v", grpcwire.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "block device") {
		t.Errorf("the message does not say what was asked for: %v", err)
	}
}

func TestDeletingAVolumeDoesNotTouchTheDataset(t *testing.T) {
	// A claim going away must not destroy the user's data. The volume only
	// ever borrowed a view of it.
	r := &fakeResolver{objects: map[string]string{"team/imagenet": "imagenet/v17"}}
	d := controller(t, r)
	var req []byte
	req = protowire.AppendString(req, 1, "team/imagenet")
	if _, err := call(t, d, csi.MethodDeleteVolume, req); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.objects["team/imagenet"]; !ok {
		t.Error("the dataset went away with the claim")
	}
}

func TestValidationAnswersNoRatherThanFailing(t *testing.T) {
	// The question was answerable and the answer is no. A failure would tell
	// the caller the driver is broken instead.
	d := controller(t, &fakeResolver{objects: map[string]string{"team/imagenet": "imagenet/v17"}})
	var req []byte
	req = protowire.AppendString(req, 1, "team/imagenet")
	req = protowire.AppendMessage(req, 3, writerMount())

	got, err := call(t, d, csi.MethodValidateVolumeCapabilities, req)
	if err != nil {
		t.Fatalf("validation failed rather than answering: %v", err)
	}
	if confirmed, why := decodeValidate(t, got); confirmed {
		t.Error("a writer was confirmed")
	} else if !strings.Contains(why, "immutable") {
		t.Errorf("no reason given: %q", why)
	}
}

func TestValidationConfirmsAReader(t *testing.T) {
	d := controller(t, &fakeResolver{objects: map[string]string{"team/imagenet": "imagenet/v17"}})
	var req []byte
	req = protowire.AppendString(req, 1, "team/imagenet")
	req = protowire.AppendMessage(req, 3, readOnlyMount())
	got, err := call(t, d, csi.MethodValidateVolumeCapabilities, req)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed, _ := decodeValidate(t, got); !confirmed {
		t.Error("a reader was not confirmed")
	}
}

func TestPublishingMountsTheObjectUnderTheExport(t *testing.T) {
	m, o := newMounter(), &fakeObserver{}
	d := node(t, m, o)
	req := publishRequest("team/imagenet", "/var/lib/kubelet/pods/abc/x", readOnlyMount(),
		map[string]string{"object": "imagenet/v17", "bytes": "12582912"})
	if _, err := call(t, d, csi.MethodNodePublishVolume, req); err != nil {
		t.Fatal(err)
	}
	if got, want := m.sourceOf("/var/lib/kubelet/pods/abc/x"), "10.0.0.9:/forebay/imagenet/v17"; got != want {
		t.Errorf("mounted %q, want %q", got, want)
	}
}

func TestPublishingTellsTheAgentOnlyAfterTheMount(t *testing.T) {
	// The observation is that a dataset is resident. Saying so before the
	// mount has happened would have the agent reclaiming against a mount that
	// then failed.
	m, o := newMounter(), &fakeObserver{}
	m.err = errors.New("the access layer is unreachable")
	d := node(t, m, o)
	req := publishRequest("team/imagenet", "/target", readOnlyMount(),
		map[string]string{"object": "imagenet/v17", "bytes": "12582912"})
	if _, err := call(t, d, csi.MethodNodePublishVolume, req); err == nil {
		t.Fatal("a failed mount was reported as success")
	}
	if got := o.record(); len(got) != 0 {
		t.Errorf("the agent was told about a mount that did not happen: %v", got)
	}
}

func TestPublishingAndUnpublishingAreReportedToTheAgent(t *testing.T) {
	m, o := newMounter(), &fakeObserver{}
	d := node(t, m, o)
	pub := publishRequest("team/imagenet", "/target", readOnlyMount(),
		map[string]string{"object": "imagenet/v17", "bytes": "12582912"})
	if _, err := call(t, d, csi.MethodNodePublishVolume, pub); err != nil {
		t.Fatal(err)
	}
	var unpub []byte
	unpub = protowire.AppendString(unpub, 1, "team/imagenet")
	unpub = protowire.AppendString(unpub, 2, "/target")
	if _, err := call(t, d, csi.MethodNodeUnpublishVolume, unpub); err != nil {
		t.Fatal(err)
	}

	want := []string{"published team/imagenet 12582912", "unpublished team/imagenet"}
	got := o.record()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
	if m.sourceOf("/target") != "" {
		t.Error("the mount is still there after unpublish")
	}
}

func TestAnObjectCannotClimbOutOfTheExport(t *testing.T) {
	// The object arrives in a message. A node plugin that trusted it would
	// mount whatever anything holding the socket named.
	m, o := newMounter(), &fakeObserver{}
	d := node(t, m, o)
	for _, object := range []string{"../etc", "../../srv/other", "/etc"} {
		req := publishRequest("team/imagenet", "/target", readOnlyMount(),
			map[string]string{"object": object})
		_, err := call(t, d, csi.MethodNodePublishVolume, req)
		if err == nil {
			t.Errorf("%q was mounted from %q", object, m.sourceOf("/target"))
			continue
		}
		if grpcwire.CodeOf(err) != grpcwire.InvalidArgument {
			t.Errorf("%q gave %v", object, grpcwire.CodeOf(err))
		}
	}
}

func TestPublishingWithoutAnObjectIsRefused(t *testing.T) {
	// The controller sets it. A publish that arrives without one is a request
	// this driver did not resolve, and mounting the export's root would give
	// a pod every dataset in the tenant.
	m, o := newMounter(), &fakeObserver{}
	d := node(t, m, o)
	req := publishRequest("team/imagenet", "/target", readOnlyMount(), map[string]string{})
	_, err := call(t, d, csi.MethodNodePublishVolume, req)
	if grpcwire.CodeOf(err) != grpcwire.InvalidArgument {
		t.Fatalf("got %v from %v", grpcwire.CodeOf(err), err)
	}
	if m.sourceOf("/target") != "" {
		t.Error("something was mounted anyway")
	}
	// Named as a missing context rather than as a bad object: the operator
	// reading this needs to look at the controller that should have set it,
	// not at the name of a dataset nobody sent.
	if !strings.Contains(err.Error(), "volume context") {
		t.Errorf("got %q, want it to point at the volume context", err)
	}
}

func TestTheNodeHalfDoesNotServeControllerMethods(t *testing.T) {
	// A node answering CreateVolume would be a second answer to a question the
	// cluster asks once.
	m, o := newMounter(), &fakeObserver{}
	d := node(t, m, o)
	_, err := call(t, d, csi.MethodCreateVolume, createRequest("pvc", "team/imagenet", readOnlyMount()))
	if grpcwire.CodeOf(err) != grpcwire.Unimplemented {
		t.Errorf("got %v", grpcwire.CodeOf(err))
	}
}

func TestTheControllerHalfDoesNotServeNodeMethods(t *testing.T) {
	d := controller(t, &fakeResolver{})
	_, err := call(t, d, csi.MethodNodePublishVolume, nil)
	if grpcwire.CodeOf(err) != grpcwire.Unimplemented {
		t.Errorf("got %v", grpcwire.CodeOf(err))
	}
}

func TestBothHalvesIdentifyThemselves(t *testing.T) {
	// Every plugin answers identity, whichever half it is: the registrar asks
	// a node plugin its name and the provisioner asks the controller.
	for _, d := range []*csi.Driver{
		controller(t, &fakeResolver{}),
		node(t, newMounter(), &fakeObserver{}),
	} {
		got, err := call(t, d, csi.MethodGetPluginInfo, nil)
		if err != nil {
			t.Fatal(err)
		}
		if name := decodeName(t, got); name != csi.Name {
			t.Errorf("called itself %q, want %q", name, csi.Name)
		}
		if _, err := call(t, d, csi.MethodProbe, nil); err != nil {
			t.Errorf("probe: %v", err)
		}
	}
}

func TestANodePluginNeedsToKnowWhereItIs(t *testing.T) {
	// Without these the node half cannot answer NodeGetInfo or build a mount
	// source, and the failure would arrive at the first pod rather than at
	// startup.
	for _, c := range []csi.Config{
		{Mounter: newMounter(), Access: "10.0.0.9", Export: "forebay"},
		{Mounter: newMounter(), NodeID: "worker-3", Export: "forebay"},
		{Mounter: newMounter(), NodeID: "worker-3", Access: "10.0.0.9"},
	} {
		if _, err := csi.New(c); err == nil {
			t.Errorf("a node plugin was built with node=%q access=%q export=%q",
				c.NodeID, c.Access, c.Export)
		}
	}
}

func TestTheKubeletIsToldWhatThisPluginIsAndWhereToCallIt(t *testing.T) {
	// The kubelet watches a directory, asks whatever socket it finds what it
	// is, and will not use a plugin whose type, name or version it does not
	// recognise. Each of those is matched as a string.
	s := grpcwire.NewServer()
	csi.NewRegistration("/var/lib/kubelet/plugins/csi.forebay.io/csi.sock", nil).Register(s)

	// Called by the literal name off the wire rather than through the
	// constant. The kubelet calls this one, and a test that used the same
	// constant the driver registered with would agree with any spelling,
	// including a wrong one: the package is pluginregistration with no
	// version in it, which is not the convention CSI's own services follow.
	got, err := callServer(t, s, "/pluginregistration.Registration/GetInfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[int]string{}
	for len(got) > 0 {
		f, rest, err := protowire.Next(got)
		if err != nil {
			t.Fatal(err)
		}
		fields[f.Number] = f.String()
		got = rest
	}
	for _, c := range []struct {
		number int
		want   string
	}{
		{1, "CSIPlugin"},
		{2, csi.Name},
		{3, "/var/lib/kubelet/plugins/csi.forebay.io/csi.sock"},
		{4, "1.0.0"},
	} {
		if fields[c.number] != c.want {
			t.Errorf("field %d was %q, want %q", c.number, fields[c.number], c.want)
		}
	}
}

func TestARefusedRegistrationIsReportedAndNotReturned(t *testing.T) {
	// The kubelet is telling this side what happened. Answering with a
	// failure would leave it retrying a call that is not the thing that went
	// wrong, and the plugin would still be unusable.
	type verdict struct {
		registered bool
		why        string
	}
	// Buffered and read with a timeout, so a driver that never passes the
	// verdict on fails the test rather than hanging it.
	seen := make(chan verdict, 1)
	s := grpcwire.NewServer()
	csi.NewRegistration("/csi/csi.sock", func(ok bool, reason string) {
		seen <- verdict{ok, reason}
	}).Register(s)

	var req []byte
	req = protowire.AppendBool(req, 1, false)
	req = protowire.AppendString(req, 2, "no such driver name")
	if _, err := callServer(t, s, "/pluginregistration.Registration/NotifyRegistrationStatus", req); err != nil {
		t.Fatalf("the kubelet's own report was answered with an error: %v", err)
	}

	var got verdict
	select {
	case got = <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the kubelet's verdict never reached the driver")
	}
	registered, why := got.registered, got.why
	if registered || why != "no such driver name" {
		t.Errorf("got registered=%v why=%q", registered, why)
	}
	// An operator has to be able to tell a plugin that is running from one
	// that is running and unusable.
	if got := csi.RegistrationStatus(registered, why); !strings.Contains(got, "refused") {
		t.Errorf("got %q", got)
	}
}
