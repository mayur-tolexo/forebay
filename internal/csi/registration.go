package csi

import (
	"context"
	"fmt"

	"github.com/mayur-tolexo/forebay/internal/grpcwire"
	"github.com/mayur-tolexo/forebay/internal/protowire"
)

// The kubelet's plugin registration, which is how a node plugin becomes usable
// rather than merely running.
//
// It is normally a sidecar's job, and it is two methods: the kubelet watches a
// directory for sockets, asks whichever it finds what it is, and says whether
// it took. Serving it here removes a container from every node and a version
// to keep in step with this one, which is worth more than the convention.
const (
	// The package is pluginregistration with no version in it. That is not
	// the convention CSI's own services follow, and it is what the kubelet
	// calls: a driver that guesses at the versioned spelling is answered
	// Unimplemented by its own transport and never registers.
	MethodGetInfo                  = "/pluginregistration.Registration/GetInfo"
	MethodNotifyRegistrationStatus = "/pluginregistration.Registration/NotifyRegistrationStatus"

	// pluginTypeCSI is what the kubelet calls a plugin of this kind. It is
	// matched as a string, so it is the spelling that matters.
	pluginTypeCSI = "CSIPlugin"
	// csiVersion is the specification version this driver answers for. The
	// kubelet checks it before it will use the plugin.
	csiVersion = "1.0.0"
)

// Registration answers the kubelet's questions about this plugin.
type Registration struct {
	// endpoint is where the kubelet should call the driver, which is a
	// different socket from the one this is served on: this one lives in the
	// directory the kubelet watches, and that one is the driver's own.
	endpoint string
	// onStatus is told what the kubelet made of the registration, so a
	// refusal is visible at the node rather than only in the kubelet's log.
	onStatus func(registered bool, why string)
}

// NewRegistration builds one.
func NewRegistration(endpoint string, onStatus func(bool, string)) *Registration {
	return &Registration{endpoint: endpoint, onStatus: onStatus}
}

// Register wires the registration methods onto a server.
func (r *Registration) Register(s *grpcwire.Server) {
	s.Register(MethodGetInfo, r.getInfo)
	s.Register(MethodNotifyRegistrationStatus, r.notify)
}

// getInfo tells the kubelet what this plugin is and where to call it.
func (r *Registration) getInfo(context.Context, []byte) ([]byte, error) {
	var out []byte
	out = protowire.AppendString(out, 1, pluginTypeCSI)
	out = protowire.AppendString(out, 2, Name)
	out = protowire.AppendString(out, 3, r.endpoint)
	out = protowire.AppendString(out, 4, csiVersion)
	return out, nil
}

// notify receives the kubelet's verdict.
//
// A refusal is reported and never returned as an error: the kubelet is telling
// this side what happened, and answering with a failure would leave it
// retrying a call that is not the thing that went wrong.
func (r *Registration) notify(_ context.Context, req []byte) ([]byte, error) {
	var registered bool
	var why string
	for len(req) > 0 {
		f, rest, err := protowire.Next(req)
		if err != nil {
			return nil, grpcwire.Errorf(grpcwire.InvalidArgument, "unreadable status: %v", err)
		}
		switch f.Number {
		case 1:
			registered = f.Bool()
		case 2:
			why = f.Text()
		}
		req = rest
	}
	if r.onStatus != nil {
		r.onStatus(registered, why)
	}
	return nil, nil
}

// RegistrationStatus renders the kubelet's verdict for an operator.
func RegistrationStatus(registered bool, why string) string {
	if registered {
		return fmt.Sprintf("the kubelet registered %s", Name)
	}
	if why == "" {
		return fmt.Sprintf("the kubelet refused %s and said nothing about why", Name)
	}
	return fmt.Sprintf("the kubelet refused %s: %s", Name, why)
}
