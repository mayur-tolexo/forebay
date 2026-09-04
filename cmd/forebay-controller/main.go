// Command forebay-controller runs the control plane, which grants capacity
// leases and decides placement, and never sits in the IO path.
//
// What it does today is one thing: it resolves the datasets a user declared
// against the durable store, and records what it found. That is the smallest
// useful half of RFC-0014, and it is the half that makes `kubectl get datasets`
// say something true.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/s3driver"
	"github.com/mayur-tolexo/forebay/internal/intent"
	"github.com/mayur-tolexo/forebay/internal/kube"
	"github.com/mayur-tolexo/forebay/internal/version"
)

const (
	accessKeyEnv = "FOREBAY_S3_ACCESS_KEY"
	secretKeyEnv = "FOREBAY_S3_SECRET_KEY"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-controller:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion    = flag.Bool("version", false, "print the build identity and exit")
		apiServer      = flag.String("api-server", "", "API server URL, defaulting to the one this pod was given")
		token          = flag.String("token", "", "bearer token, defaulting to this pod's own")
		namespace      = flag.String("namespace", "", "namespace to watch, empty for every one")
		interval       = flag.Duration("interval", 30*time.Second, "how often datasets are resolved against the store")
		endpoint       = flag.String("s3-endpoint", "", "scheme and host of the durable backend")
		bucket         = flag.String("s3-bucket", "", "bucket the backend serves from")
		region         = flag.String("s3-region", "", "region the backend signs for")
		knowsRacks     = flag.Bool("fleet-knows-racks", false, "whether topology can name the rack a node is in, which rack tolerance needs and no backend can supply")
		agentService   = flag.String("agent-service", "", "the headless service the node agents answer on. Set it to publish node residency labels, which needs this controller to be able to patch nodes")
		agentNS        = flag.String("agent-namespace", "", "namespace that service is in")
		leaseTokenFile = flag.String("lease-token-file", "", "file holding the token the node agents require before they will consider a lease proposal. Set it to plan tier capacity from what datasets declare")
		nodeShare      = flag.Float64("node-share", 0.25, "the most of a node's free space this will ask for. A node exists to run compute, and the pool arithmetic is its floor rather than something a planner should aim at")
		floor          = flag.String("durability-floor", "", "the least durability every dataset here must require, which can only raise what a user declared and never lower it")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("forebay-controller", version.String())
		return nil
	}
	if err := checkFlags(*endpoint, *bucket, *interval); err != nil {
		return err
	}

	backend, err := openBackend(*endpoint, *bucket, *region)
	if err != nil {
		return err
	}
	client, err := openClient(*apiServer, *token)
	if err != nil {
		return err
	}

	// What an intent resolves against: the backend's own declaration, and
	// whether this fleet can name a rack. The second is not a backend's to
	// answer and an intent needing it fails for a reason no backend could fix.
	resolvable := kube.Resolvable{
		Backend: backend,
		Fleet:   intent.Fleet{KnowsRacks: *knowsRacks},
		Floor:   intent.Floor{Durability: intent.Durability(*floor)},
	}
	// Refused at startup, since a floor naming a durability that does not
	// exist would raise nothing and leave an administrator believing a
	// requirement was in force.
	if err := resolvable.Floor.Validate(); err != nil {
		return err
	}

	resource := kube.DatasetResource
	resource.Namespace = *namespace
	where := *namespace
	if where == "" {
		where = "every namespace"
	}
	fmt.Printf("forebay-controller %s, resolving datasets in %s against %s every %s\n",
		version.String(), where, *bucket, *interval)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Nil unless asked for. Publishing residency needs permission to patch
	// every node, which is the widest thing this project asks for, so an
	// operator turns it on rather than finding it on.
	// Read before the loop starts, so a controller pointed at a token file it
	// cannot read stops rather than proposing to every node and being refused
	// by all of them for a reason nobody looks at.
	planToken, err := readToken(*leaseTokenFile)
	if err != nil {
		return err
	}
	var plan *proposer
	if planToken != "" && *agentService != "" {
		if *nodeShare <= 0 || *nodeShare > 1 {
			return fmt.Errorf("--node-share must be a share of a node, got %v", *nodeShare)
		}
		plan = &proposer{
			client: client, token: planToken, service: *agentService,
			namespace: *agentNS, timeout: defaultResidencyTimeout, share: *nodeShare,
		}
		fmt.Printf("planning tier capacity across the agents on %s/%s, up to %.0f%% of each node's free space\n",
			*agentNS, *agentService, *nodeShare*100)
	}

	var residency *residencyPass
	if *agentService != "" {
		residency = &residencyPass{
			client:    client,
			http:      &http.Client{Timeout: defaultResidencyTimeout},
			service:   *agentService,
			namespace: *agentNS,
		}
		fmt.Printf("labelling nodes from the agents on %s/%s\n", *agentNS, *agentService)
	}

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		if residency != nil {
			// Separately from the datasets, and neither stops the other: a
			// cluster whose agents are unreachable should still have its
			// dataset statuses reconciled.
			if n, err := residency.run(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "forebay-controller: labelling nodes:", err)
			} else if n > 0 {
				fmt.Printf("relabelled %d node(s)\n", n)
			}
		}
		if plan != nil {
			// After the datasets are reconciled below on the previous pass,
			// which is where their sizes come from: planning against a list
			// nothing has resolved yet would ask for capacity for datasets
			// whose size is still unknown.
			if err := planCapacity(ctx, plan, client, resource, resolvable); err != nil {
				fmt.Fprintln(os.Stderr, "forebay-controller: planning capacity:", err)
			}
		}
		if n, err := reconcile(ctx, client, resource, backend, resolvable); err != nil {
			// A pass that failed does not end the controller: the API server
			// being away is a condition it is expected to sit through, and
			// stopping would need something else to start it again.
			fmt.Fprintln(os.Stderr, "forebay-controller: pass failed:", err)
		} else if n > 0 {
			fmt.Printf("recorded %d dataset(s)\n", n)
		}
		select {
		case <-ctx.Done():
			fmt.Println("forebay-controller: stopped")
			return nil
		case <-tick.C:
		}
	}
}

// readToken reads the token the agents require, trimming what a file written
// by an operator or mounted from a secret carries.
func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the lease token: %w", err)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("the lease token in %s is empty", path)
	}
	return token, nil
}

// planCapacity asks the nodes to cover what the datasets declare.
func planCapacity(ctx context.Context, p *proposer, c *kube.Client, r kube.Resource, resolvable kube.Resolvable) error {
	var list kube.DatasetList
	if err := c.List(ctx, r, &list); err != nil {
		return err
	}
	want := wanted(list, resolvable.Floor)
	if len(want) == 0 {
		return nil
	}
	granted, refused, err := p.propose(ctx, want)
	if granted > 0 || refused > 0 {
		fmt.Println(describe(granted, refused))
	}
	return err
}

// checkFlags rejects a controller that could not resolve anything.
func checkFlags(endpoint, bucket string, interval time.Duration) error {
	switch {
	case endpoint == "" || bucket == "":
		return fmt.Errorf("--s3-endpoint and --s3-bucket are required, since resolving a dataset means asking the store")
	case interval <= 0:
		return fmt.Errorf("--interval must be positive, got %s", interval)
	}
	return nil
}

// reconcile resolves every dataset once, returning how many it had to write.
//
// It writes only what changed. A controller that patched every object on every
// pass would put a cluster's worth of writes into etcd for nothing, and the
// cost lands on everything else using it rather than on this.
func reconcile(ctx context.Context, c *kube.Client, r kube.Resource, store kube.Sizer, resolvable kube.Resolvable) (int, error) {
	var list kube.DatasetList
	if err := c.List(ctx, r, &list); err != nil {
		return 0, err
	}
	var (
		wrote  int
		failed []error
	)
	for _, d := range list.Items {
		status, err := kube.Resolve(ctx, store, d)
		if err == nil {
			kube.ResolveIntent(d, resolvable, &status)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "forebay-controller: %s: %v\n", d.Metadata.Name, err)
			continue
		}
		if !kube.Changed(d.Status, status) {
			continue
		}
		one := r
		if one.Namespace == "" {
			one.Namespace = d.Metadata.Namespace
		}
		switch err := c.PatchStatus(ctx, one, d.Metadata.Name, status); {
		case err == nil:
			wrote++
		case kube.NotFound(err):
			// Deleted between the list and the write, which is ordinary.
		default:
			// Kept and carried rather than returned. One object the API
			// server would not take must not stop the rest of the cluster
			// being reconciled, and a pass that gave up on the first failure
			// would leave everything after it unresolved for as long as that
			// one object stayed broken.
			failed = append(failed, fmt.Errorf("%s/%s: %w", d.Metadata.Namespace, d.Metadata.Name, err))
		}
	}
	return wrote, errors.Join(failed...)
}

// openBackend points the controller at the store it resolves against.
func openBackend(endpoint, bucket, region string) (*driver.Backend, error) {
	access, secret := os.Getenv(accessKeyEnv), os.Getenv(secretKeyEnv)
	if access == "" || secret == "" {
		return nil, fmt.Errorf("the store's credentials come from %s and %s", accessKeyEnv, secretKeyEnv)
	}
	d, err := s3driver.New(s3driver.Config{
		Endpoint: endpoint, Bucket: bucket, Region: region,
		AccessKey: access, SecretKey: secret,
		HTTPClient: &http.Client{Timeout: time.Minute},
	})
	if err != nil {
		return nil, err
	}
	return driver.Open(d)
}

// openClient builds the API client, from flags when given and from the pod's
// own credentials when not.
func openClient(apiServer, token string) (*kube.Client, error) {
	if apiServer == "" {
		cfg, err := kube.InCluster()
		if err != nil {
			return nil, err
		}
		return kube.New(cfg)
	}
	return kube.New(kube.Config{Host: apiServer, Token: token, Timeout: 30 * time.Second})
}
