package kubelet_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/kubelet"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// fakeKubelet serves the fixtures captured from the dev cluster's kubelet, so
// the shapes under test are ones a real kubelet produced.
func fakeKubelet(t *testing.T, wantToken string) *kubelet.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var file string
		switch r.URL.Path {
		case "/pods":
			file = "testdata/pods.json"
		case "/stats/summary":
			file = "testdata/stats.json"
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("reading %s: %v", file, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	return kubelet.New(host, port, wantToken)
}

func hostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	trimmed := strings.TrimPrefix(url, "https://")
	host, portStr, found := strings.Cut(trimmed, ":")
	if !found {
		t.Fatalf("no port in %s", url)
	}
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

func TestPodsAreReadFromTheShapesARealKubeletProduces(t *testing.T) {
	c := fakeKubelet(t, "tok")
	// One pod in the fixture has a request nothing can read, so the error is
	// expected and the other six still come back.
	pods, err := c.Pods(context.Background())
	if err == nil {
		t.Fatal("the unreadable pod was not reported")
	}
	if len(pods) != 6 {
		t.Fatalf("got %d pods, want the 6 readable ones in the fixture", len(pods))
	}

	// By prefix, because the generated suffix on a real pod name is exactly
	// the kind of thing that gets copied wrong out of truncated output.
	find := func(prefix string) kubelet.Pod {
		t.Helper()
		for _, p := range pods {
			if strings.HasPrefix(p.Name, prefix) {
				return p
			}
		}
		t.Fatalf("no pod named %s* in the fixture", prefix)
		return kubelet.Pod{}
	}

	// A pod that declares nothing contributes nothing, which is most of them.
	if p := find("virt-handler"); p.Declared != 0 || p.Unwritten() != 0 {
		t.Errorf("a pod declaring nothing reported %d declared", p.Declared)
	}
	// One container declares, the other does not, and the pod is the sum.
	if p := find("neevai-runner"); p.Declared != 10<<30 {
		t.Errorf("declared = %d, want 10GiB", p.Declared)
	}
	// A failed pod will write nothing more.
	if p := find("neev-gpu"); p.Live {
		t.Error("a Failed pod was counted as able to write")
	}
	// Usage comes from the other endpoint and is matched by namespace and name.
	if p := find("fb-kp"); p.Used == 0 {
		t.Error("usage was not joined onto the pod from stats/summary")
	}
}

func TestAnInitContainerDoesNotAddToTheAppContainers(t *testing.T) {
	// Verified against a real node: a pod with a 20Gi init container and a
	// 1Gi app container moved the node's allocated ephemeral-storage by 20Gi,
	// not 21Gi. Init runs and exits before the app starts, so the pod is
	// charged the larger of the two. Summing them would reclaim for a demand
	// that cannot exist, and the overstatement grows with every init
	// container.
	pods, _ := fakeKubelet(t, "tok").Pods(context.Background())
	for _, p := range pods {
		if p.Name != "fb-init" {
			continue
		}
		if p.Declared != 20<<30 {
			t.Errorf("declared = %d, want the 20GiB init container rather than 21GiB summed", p.Declared)
		}
		return
	}
	t.Fatal("fb-init missing from the fixture")
}

func TestASidecarAddsToTheAppContainers(t *testing.T) {
	// A sidecar is declared as an init container but keeps running alongside
	// the app, so it is the one init container that does add. The fixture
	// pod is a 1Gi app, a 3Gi sidecar and a 2Gi ordinary init container, and
	// Kubernetes charges max(1+3, 2).
	pods, _ := fakeKubelet(t, "tok").Pods(context.Background())
	for _, p := range pods {
		if p.Name != "fb-sidecar" {
			continue
		}
		if p.Declared != 4<<30 {
			t.Errorf("declared = %d, want 4GiB", p.Declared)
		}
		return
	}
	t.Fatal("fb-sidecar missing from the fixture")
}

func TestOnePodWithAnUnreadableRequestDoesNotHideTheRest(t *testing.T) {
	// The failure that matters: refusing the read outright would let one pod
	// nobody cares about blind the input for every other pod on the node.
	pods, err := fakeKubelet(t, "tok").Pods(context.Background())
	if err == nil {
		t.Fatal("the unreadable pod was not reported at all")
	}
	if !strings.Contains(err.Error(), "fb-unreadable") {
		t.Errorf("the error does not name the pod that was dropped: %v", err)
	}
	for _, p := range pods {
		if p.Name == "fb-unreadable" {
			t.Error("a pod whose request could not be read was counted anyway")
		}
	}
	if len(pods) < 6 {
		t.Errorf("got %d pods, want every readable pod despite the one bad request", len(pods))
	}
}

func TestTheNodeFilesystemIsReported(t *testing.T) {
	// This is what tells the caller the kubelet is watching the same
	// filesystem the pools are on. Without it the pod input can be acting on
	// a device Forebay does not lend from.
	capacity, available, err := fakeKubelet(t, "tok").NodeFS(context.Background())
	if err != nil {
		t.Fatalf("NodeFS: %v", err)
	}
	if capacity != 2013991550976 || available != 1413445918720 {
		t.Errorf("NodeFS = %d, %d; want what the fixture reports", capacity, available)
	}
}

func TestUnwrittenNeverGoesNegative(t *testing.T) {
	// A pod over its request is the kubelet's problem, not a claim on more
	// capacity, and a negative would subtract from a real shortfall.
	p := kubelet.Pod{Declared: 1 << 20, Used: 4 << 20}
	if got := p.Unwritten(); got != 0 {
		t.Errorf("Unwritten = %d for a pod over its request, want 0", got)
	}
}

func TestAWrongTokenIsAnErrorRatherThanAnEmptyAnswer(t *testing.T) {
	// An unauthorised read that returned no pods would look exactly like a
	// node with nothing declared, and the watch would quietly lose an input.
	c := fakeKubelet(t, "right")
	wrong := kubelet.New("127.0.0.1", 1, "wrong")
	if _, err := wrong.Pods(context.Background()); err == nil {
		t.Error("an unreachable kubelet returned no error")
	}
	// The right token returns pods. It also returns the fixture's unreadable
	// pod as an error, which is a different thing from being turned away.
	if pods, _ := c.Pods(context.Background()); len(pods) == 0 {
		t.Error("the right token returned no pods")
	}
}

func TestTheSourceReportsWhatPodsHaveNotYetWritten(t *testing.T) {
	// The whole point of this input: a pod holding a large request and one
	// byte on disk is invisible to free space until it writes.
	src := mustSource(t, fakeKubelet(t, "tok"), nodeCapacity)
	cfg := agent.WatchConfig{Headroom: 64 << 30}

	need, err := src.Observe(context.Background(), cfg, 60<<30)
	// The fixture has an unreadable pod, so the answer is a floor and says so.
	var partial *agent.Partial
	if !errors.As(err, &partial) {
		t.Fatalf("observing: want a partial read, got %v", err)
	}
	// Live pods hold 10GiB, 2GiB, 20GiB and 4GiB, less what two of them have
	// already written, which free space can see for itself.
	const written = 798720 + 1105920
	if want := pool.Bytes(64<<30) - (60<<30 - (36<<30 - written)); need != want {
		t.Errorf("need = %d, want %d", need, want)
	}
	if src.Name() != "pod requests" {
		t.Errorf("Name = %q", src.Name())
	}
}

func TestAPartialReadStillReportsTheNeedItCouldSee(t *testing.T) {
	// A shortfall the readable pods imply is real whether or not another pod
	// parsed. Returning zero here would throw away a need the node has.
	src := mustSource(t, fakeKubelet(t, "tok"), nodeCapacity)
	need, err := src.Observe(context.Background(), agent.WatchConfig{Headroom: 0}, 0)
	var partial *agent.Partial
	if !errors.As(err, &partial) {
		t.Fatalf("want a partial read, got %v", err)
	}
	if need <= 0 {
		t.Errorf("need = %s, want the shortfall the readable pods imply", need)
	}
}

func TestTheSourceIgnoresPodsThatWillNotWriteAgain(t *testing.T) {
	// The fixture holds a Failed pod declaring 50 GiB. Counting it would
	// reclaim a job's cache for a demand that no longer exists.
	src := mustSource(t, fakeKubelet(t, "tok"), nodeCapacity)
	need, _ := src.Observe(context.Background(), agent.WatchConfig{Headroom: 0}, 0)
	// Live pods hold 36GiB between them, less the little they have written.
	if need > 36<<30 || need < 35<<30 {
		t.Errorf("need = %s, want roughly the 36GiB the live pods hold", need)
	}
}

func TestAnUnreachableKubeletIsNotAPartialRead(t *testing.T) {
	// A source that saw nothing has no floor to offer, so it must fail
	// outright rather than report a zero need the watch would act on.
	src := mustSource(t, kubelet.New("127.0.0.1", 1, "tok"), nodeCapacity)
	_, err := src.Observe(context.Background(), agent.WatchConfig{Headroom: 1 << 30}, 0)
	var partial *agent.Partial
	if err == nil || errors.As(err, &partial) {
		t.Errorf("want an outright failure, got %v", err)
	}
}

// nodeCapacity is the size of the fixture's filesystem, which is the ceiling
// on what pods on it can still write.
const nodeCapacity = 2013991550976

// kubeletServingPods answers /pods with exactly the given items, for the cases
// that need a pod shape the shared fixture should not carry.
func kubeletServingPods(t *testing.T, items string) *kubelet.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pods" {
			fmt.Fprintf(w, `{"items":[%s]}`, items)
			return
		}
		fmt.Fprintf(w, `{"node":{"fs":{"capacityBytes":%d,"availableBytes":1}},"pods":[]}`, nodeCapacity)
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	return kubelet.New(host, port, "")
}

// clampedPod is what the API server stores for a request larger than a byte
// count holds: it clamps to the largest signed 64-bit value rather than
// refusing, so this is a value a real node serves.
const clampedPod = `{"metadata":{"name":"big","namespace":"d"},` +
	`"spec":{"containers":[{"resources":{"requests":{"ephemeral-storage":"9223372036854775807"}}}]},` +
	`"status":{"phase":"Running"}}`

func TestAnImpossibleRequestIsDroppedNotActedOn(t *testing.T) {
	// The API server clamps an over-large request to the largest signed
	// 64-bit value rather than refusing it, so this arrives on a real node.
	// Counting it wraps negative and blinds the input; capping it at the
	// filesystem reclaims every lease on the node for a demand that does not
	// exist. It is neither: the scheduler would never have admitted a pod
	// asking for 9 EiB of a 1.8 TiB node, so it is not evidence, and it is
	// dropped the way an unreadable request is.
	src := mustSource(t, kubeletServingPods(t, clampedPod), nodeCapacity)
	need, err := src.Observe(context.Background(), agent.WatchConfig{Headroom: 64 << 30}, 60<<30)

	var partial *agent.Partial
	if !errors.As(err, &partial) {
		t.Fatalf("the dropped pod was not reported: %v", err)
	}
	if !strings.Contains(err.Error(), "more than the") {
		t.Errorf("the complaint does not say why the pod was dropped: %v", err)
	}
	// The pod contributes nothing, so this is what free space alone sees.
	if want := pool.Bytes(64<<30) - 60<<30; need != want {
		t.Errorf("need = %s, want %s: the impossible pod must add nothing", need, want)
	}
}

// mustSource builds a source or fails the test.
func mustSource(t *testing.T, c *kubelet.Client, capacity int64) *kubelet.Source {
	t.Helper()
	src, err := kubelet.NewSource(c, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestASourceWithoutAFilesystemSizeIsRefused(t *testing.T) {
	// Without a size there is no way to tell a large request from an
	// impossible one, and a source that silently counts nothing is the
	// failure this whole ceiling exists to prevent.
	if _, err := kubelet.NewSource(fakeKubelet(t, "tok"), 0); err == nil {
		t.Error("a source was built with no filesystem size")
	}
}

func TestTwoClampedRequestsInOnePodDoNotWrap(t *testing.T) {
	// The wrap can happen inside a single pod, before the source sees it.
	const twoContainers = `{"metadata":{"name":"big","namespace":"d"},"spec":{"containers":[` +
		`{"resources":{"requests":{"ephemeral-storage":"9223372036854775807"}}},` +
		`{"resources":{"requests":{"ephemeral-storage":"9223372036854775807"}}}]},` +
		`"status":{"phase":"Running"}}`
	pods, err := kubeletServingPods(t, twoContainers).Pods(context.Background())
	if err != nil {
		t.Fatalf("reading pods: %v", err)
	}
	if pods[0].Declared <= 0 {
		t.Errorf("declared = %d, want a saturated count rather than a wrapped one", pods[0].Declared)
	}
}

func TestEveryPodBeingUnreadableIsAnOutrightFailure(t *testing.T) {
	// With nothing read there is no floor to offer, so this must fail rather
	// than report a zero need the watch would take as no pressure.
	const bad = `{"metadata":{"name":"b","namespace":"d"},` +
		`"spec":{"containers":[{"resources":{"requests":{"ephemeral-storage":"banana"}}}]},` +
		`"status":{"phase":"Running"}}`
	src := mustSource(t, kubeletServingPods(t, bad), nodeCapacity)
	_, err := src.Observe(context.Background(), agent.WatchConfig{Headroom: 1 << 30}, 0)
	var partial *agent.Partial
	if err == nil || errors.As(err, &partial) {
		t.Errorf("want an outright failure, got %v", err)
	}
}
