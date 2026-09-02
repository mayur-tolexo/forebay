// Package kubelet reads what this node's kubelet knows about the pods bound
// to it.
//
// The kubelet rather than the API server, because RFC-0004 requires that
// reclamation never needs the control plane: a partition must not be able to
// stop a node giving compute its disk back.
//
// The types here are the smallest subset of the kubelet's responses that
// answers one question, which is why this package adds no dependency.
package kubelet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// tokenPath is where a pod finds its own service account token.
const tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Client talks to one node's kubelet.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New points a client at a kubelet.
//
// The kubelet serves a certificate signed for the node rather than for the
// address a pod reaches it on, so verification is skipped. What authorises the
// call is the bearer token, and what bounds the damage is that the token
// grants only nodes/proxy and nodes/stats.
func New(host string, port int, token string) *Client {
	return &Client{
		base:  fmt.Sprintf("https://%s:%d", host, port),
		token: token,
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
}

// TokenFromFile reads the pod's own service account token.
func TokenFromFile(path string) (string, error) {
	if path == "" {
		path = tokenPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("kubelet: reading service account token: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// get fetches one kubelet endpoint into v.
func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("kubelet: building request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kubelet: fetching %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kubelet: %s returned %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("kubelet: decoding %s: %w", path, err)
	}
	return nil
}

// podList is the part of the kubelet's /pods response this package reads.
type podList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Containers     []container `json:"containers"`
			InitContainers []container `json:"initContainers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

type container struct {
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
	// RestartPolicy is set only on an init container, and only to mark it a
	// sidecar, which is the one kind that keeps running alongside the app.
	RestartPolicy string `json:"restartPolicy"`
}

// request reads this container's ephemeral-storage request. A container that
// asks for nothing is not an error, it is most of them.
func (c container) request() (int64, error) {
	q, ok := c.Resources.Requests["ephemeral-storage"]
	if !ok {
		return 0, nil
	}
	return ParseQuantity(q)
}

// declared is what Kubernetes charges a pod for ephemeral storage.
//
// Init containers run and exit before the app containers start, so their
// requests do not add to the app containers': the pod is charged the larger of
// the two. Summing them instead would reclaim capacity for a demand that
// cannot exist, and the overstatement grows with every init container.
//
// A sidecar is the exception. It is declared as an init container but keeps
// running, so it is charged alongside the app containers.
func declared(regular, init []container) (int64, error) {
	var app, biggest int64
	for _, c := range regular {
		n, err := c.request()
		if err != nil {
			return 0, err
		}
		app = addSaturating(app, n)
	}
	for _, c := range init {
		n, err := c.request()
		if err != nil {
			return 0, err
		}
		if c.RestartPolicy == "Always" {
			app = addSaturating(app, n)
			continue
		}
		if n > biggest {
			biggest = n
		}
	}
	if biggest > app {
		return biggest, nil
	}
	return app, nil
}

// summary is the part of /stats/summary this package reads.
type summary struct {
	Node struct {
		FS struct {
			AvailableBytes int64 `json:"availableBytes"`
			CapacityBytes  int64 `json:"capacityBytes"`
		} `json:"fs"`
	} `json:"node"`
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Ephemeral struct {
			UsedBytes int64 `json:"usedBytes"`
		} `json:"ephemeral-storage"`
	} `json:"pods"`
}

// Pod is one pod bound to this node, reduced to what pressure needs.
type Pod struct {
	Namespace string
	Name      string
	// Declared is the ephemeral-storage this pod asked for, across every
	// container.
	Declared int64
	// Used is what it has written so far, when the kubelet reported it.
	Used int64
	// Live reports whether it can still write. A pod that has finished or
	// failed will not, so counting its request would reclaim for a demand
	// that no longer exists.
	Live bool
}

// Unwritten is what this pod may still write: what it asked for, less what it
// has already used. Never negative, since a pod over its request is a problem
// for the kubelet rather than a claim on more capacity.
func (p Pod) Unwritten() int64 {
	if n := p.Declared - p.Used; n > 0 {
		return n
	}
	return 0
}

// Pods reports the pods bound to this node.
func (c *Client) Pods(ctx context.Context) ([]Pod, error) {
	var list podList
	if err := c.get(ctx, "/pods", &list); err != nil {
		return nil, err
	}
	var stats summary
	if err := c.get(ctx, "/stats/summary", &stats); err != nil {
		return nil, err
	}
	used := make(map[string]int64, len(stats.Pods))
	for _, p := range stats.Pods {
		used[p.PodRef.Namespace+"/"+p.PodRef.Name] = p.Ephemeral.UsedBytes
	}

	out := make([]Pod, 0, len(list.Items))
	var skipped []string
	for _, item := range list.Items {
		key := item.Metadata.Namespace + "/" + item.Metadata.Name
		n, err := declared(item.Spec.Containers, item.Spec.InitContainers)
		if err != nil {
			// The pod is dropped rather than guessed at, and the caller is
			// told. Failing the whole read instead would let one pod nobody
			// cares about hide every other pod on the node.
			skipped = append(skipped, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		out = append(out, Pod{
			Namespace: item.Metadata.Namespace,
			Name:      item.Metadata.Name,
			Declared:  n,
			Used:      used[key],
			Live:      item.Status.Phase == "Running" || item.Status.Phase == "Pending",
		})
	}
	if len(skipped) > 0 {
		return out, fmt.Errorf("kubelet: %d pod(s) had a request this could not read, so what they hold is not counted: %s",
			len(skipped), strings.Join(skipped, "; "))
	}
	return out, nil
}

// NodeFS reports the size of the filesystem the kubelet charges pods for
// ephemeral storage against.
//
// This is what makes the pod input safe to act on. A pod's writes only ever
// take space from Forebay's pools if the two are the same filesystem, and
// where they are not, every reclaim the pod input drives is for pressure that
// cannot happen. Capacity is what tells them apart, since a node with the
// pools on a second device reports a different size here.
func (c *Client) NodeFS(ctx context.Context) (capacity, available int64, err error) {
	var stats summary
	if err := c.get(ctx, "/stats/summary", &stats); err != nil {
		return 0, 0, err
	}
	if stats.Node.FS.CapacityBytes == 0 {
		return 0, 0, fmt.Errorf("kubelet: reported no size for the node filesystem")
	}
	return stats.Node.FS.CapacityBytes, stats.Node.FS.AvailableBytes, nil
}
