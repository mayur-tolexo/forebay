// Command forebay-csi serves the Container Storage Interface, so a job can
// mount a dataset without knowing Forebay is there.
//
// It runs either half or both. The controller half resolves a dataset against
// the API server and allocates nothing; the node half mounts one from the
// access layer, read-only. RFC-0014 keeps them apart because they are trusted
// differently: the controller reads the cluster, and a node plugin runs on
// every node and should hold no cluster credentials at all.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mayur-tolexo/forebay/internal/csi"
	"github.com/mayur-tolexo/forebay/internal/grpcwire"
	"github.com/mayur-tolexo/forebay/internal/kube"
	"github.com/mayur-tolexo/forebay/internal/version"
	"github.com/mayur-tolexo/forebay/internal/volumes"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-csi:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion = flag.Bool("version", false, "print the build identity and exit")
		socket      = flag.String("socket", "/csi/csi.sock", "the Unix socket the orchestrator calls this on")
		controller  = flag.Bool("controller", false, "serve the controller half, which resolves datasets and needs the API server")
		nodePlugin  = flag.Bool("node", false, "serve the node half, which mounts datasets on this node")
		nodeID      = flag.String("node-id", "", "what this node is called to the orchestrator, which the node half reports")
		access      = flag.String("access", "", "host or address of the access layer this node mounts datasets from")
		export      = flag.String("export", "forebay", "the path the access layer publishes, under which a dataset's object is a directory")
		mountFlags  = flag.String("mount-flags", "vers=4.1,hard,nolock", "options passed to every mount, on top of the read-only this driver always applies")
		apiServer   = flag.String("api-server", "", "API server URL, defaulting to the one this pod was given")
		token       = flag.String("token", "", "bearer token, defaulting to this pod's own")
		agent       = flag.String("agent", "", "the node agent's address. Set it to report volume requests, which is the third input to the agent's pressure watch")
		agentToken  = flag.String("agent-token-file", "", "file holding the token the node agent requires before it will accept a report")
		timeout     = flag.Duration("timeout", 10*time.Second, "bounds one call to the API server or the agent")
		regSocket   = flag.String("registration-socket", "", "a socket in the directory the kubelet watches for plugins. Set it on a node plugin to register with the kubelet directly, instead of running a registrar beside it")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return nil
	}
	if !*controller && !*nodePlugin {
		return errors.New("nothing to serve: pass --controller, --node, or both")
	}

	cfg := csi.Config{
		NodeID:  *nodeID,
		Version: version.String(),
		Access:  *access,
		Export:  *export,
	}
	for _, f := range strings.Split(*mountFlags, ",") {
		if f = strings.TrimSpace(f); f != "" {
			cfg.MountFlags = append(cfg.MountFlags, f)
		}
	}

	if *controller {
		client, err := apiClient(*apiServer, *token, *timeout)
		if err != nil {
			return err
		}
		cfg.Resolver = csi.NewDatasets(client)
	}
	if *nodePlugin {
		cfg.Mounter = csi.NewNFS()
		observer, err := reporter(*agent, *agentToken, *timeout)
		if err != nil {
			return err
		}
		cfg.Observer = observer
	}

	d, err := csi.New(cfg)
	if err != nil {
		return err
	}

	// Removed before binding. A socket file outlives the process that made
	// it, so a plugin that restarted would find its own address taken and
	// never come back.
	if err := os.Remove(*socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing %s: %w", *socket, err)
	}
	l, err := net.Listen("unix", *socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *socket, err)
	}

	s := grpcwire.NewServer()
	d.Register(s)

	fmt.Printf("%s serving %s on %s\n", csi.Name, halves(*controller, *nodePlugin), *socket)
	if *nodePlugin {
		fmt.Printf("mounting datasets from %s:/%s with ro,%s\n", *access, *export, strings.Join(cfg.MountFlags, ","))
		if *agent == "" {
			fmt.Println("no --agent, so volume requests are not reported and the agent's watch keeps two of its three inputs")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()

	if *regSocket != "" {
		if !*nodePlugin {
			return errors.New("--registration-socket registers a node plugin with the kubelet, and this is not serving one")
		}
		stopReg, err := serveRegistration(*regSocket, *socket)
		if err != nil {
			return err
		}
		defer stopReg()
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Given a moment to finish what it is holding: a publish cut off
		// halfway leaves a mount the orchestrator does not know about.
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdown); err != nil {
			return err
		}
		return <-done
	}
}

// serveRegistration answers the kubelet on a socket in the directory it
// watches, which is what makes it use the plugin at all.
//
// A second server on a second socket, because the two are different surfaces:
// the kubelet finds this one by watching a directory, and calls the driver on
// the endpoint this one names.
func serveRegistration(socket, endpoint string) (func(), error) {
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing %s: %w", socket, err)
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", socket, err)
	}
	s := grpcwire.NewServer()
	csi.NewRegistration(endpoint, func(registered bool, why string) {
		// Printed either way. A plugin the kubelet refused is running and
		// unusable, which looks exactly like one that is working until a pod
		// waits forever for a volume.
		fmt.Println(csi.RegistrationStatus(registered, why))
	}).Register(s)

	go func() {
		if err := s.Serve(l); err != nil {
			fmt.Fprintln(os.Stderr, "forebay-csi: registration:", err)
		}
	}()
	fmt.Printf("offering %s to the kubelet on %s\n", csi.Name, socket)
	return func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(shutdown)
	}, nil
}

// apiClient builds the controller's connection to the cluster.
func apiClient(host, token string, timeout time.Duration) (*kube.Client, error) {
	cfg, err := kube.InCluster()
	if err != nil && (host == "" || token == "") {
		return nil, fmt.Errorf("the controller needs the API server, and this is not in a cluster: %w", err)
	}
	if host != "" {
		cfg.Host = host
	}
	if token != "" {
		cfg.Token = token
	}
	cfg.Timeout = timeout
	return kube.New(cfg)
}

// reporter builds the node half's link to the agent.
//
// Nothing is required. A node plugin with no agent still mounts datasets: the
// report is an observation the agent would like and not a permission the
// driver needs, and making the mount depend on it would be exactly the
// dependency RFC-0014 rules out.
func reporter(addr, tokenFile string, timeout time.Duration) (csi.Observer, error) {
	if addr == "" {
		return nil, nil
	}
	if tokenFile == "" {
		return nil, errors.New("--agent needs --agent-token-file, since the agent refuses a report without the token")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading the agent token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("%s is empty, and an empty token is not one", tokenFile)
	}
	return &reports{client: volumes.NewClient(addr, token, timeout), timeout: timeout}, nil
}

// reports sends volume requests to the agent.
//
// Failures are printed and never returned. The driver reports and never asks,
// so an agent that is down costs the watch an observation rather than costing
// a pod its mount.
type reports struct {
	client  *volumes.Client
	timeout time.Duration
}

func (r *reports) Published(id string, bytes int64) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.client.Published(ctx, id, bytes); err != nil {
		fmt.Fprintf(os.Stderr, "forebay-csi: telling the agent about %s: %v\n", id, err)
	}
}

func (r *reports) Unpublished(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.client.Unpublished(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "forebay-csi: telling the agent %s is gone: %v\n", id, err)
	}
}

// halves names what is being served, for the line an operator reads at
// startup.
func halves(controller, node bool) string {
	switch {
	case controller && node:
		return "both halves"
	case controller:
		return "the controller half"
	default:
		return "the node half"
	}
}
