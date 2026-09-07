package volumes_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/volumes"
)

func TestARepeatedPublishIsOneVolume(t *testing.T) {
	// The orchestrator repeats a publish it did not hear the answer to. A
	// running total would climb with the retries and reclaim against demand
	// that was only ever counted twice.
	r := volumes.NewRegistry()
	for i := 0; i < 5; i++ {
		r.Published("team/imagenet", 8<<20)
	}
	count, bytes := r.Requested()
	if count != 1 || bytes != 8<<20 {
		t.Errorf("got %d volumes of %s", count, pool.Bytes(bytes))
	}
}

func TestUnpublishingTakesTheDemandAwayAgain(t *testing.T) {
	r := volumes.NewRegistry()
	r.Published("a", 4<<20)
	r.Published("b", 2<<20)
	r.Unpublished("a")
	count, bytes := r.Requested()
	if count != 1 || bytes != 2<<20 {
		t.Errorf("got %d volumes of %s", count, pool.Bytes(bytes))
	}
}

func TestAVolumeOfUnknownSizeIsStillAVolume(t *testing.T) {
	// Zero means the size was not known, not that the volume is not there.
	// Dropping it would make a node that had been asked for something look
	// like one that had been asked for nothing.
	r := volumes.NewRegistry()
	r.Published("a", 0)
	if count, _ := r.Requested(); count != 1 {
		t.Errorf("got %d volumes", count)
	}
}

func TestATotalThatWouldWrapSaturates(t *testing.T) {
	// The sizes come from a control plane. A wrapped total reads as almost no
	// demand at all, which is the one answer that must not come out of an
	// over-large one.
	r := volumes.NewRegistry()
	r.Published("a", 1<<62)
	r.Published("b", 1<<62)
	r.Published("c", 1<<62)
	r.Published("d", 1<<62)
	if _, bytes := r.Requested(); bytes != 1<<63-1 {
		t.Errorf("got %d", bytes)
	}
}

func TestTheShortfallIsWhatIsLeftOnceTheVolumesAreResident(t *testing.T) {
	r := volumes.NewRegistry()
	r.Published("team/imagenet", 6<<30)
	s := volumes.NewSource(r)

	cfg := agent.WatchConfig{Headroom: 2 << 30, Interval: time.Second}
	// 8 GiB free, 6 GiB about to be wanted, so 2 GiB would be left and the
	// floor is 2 GiB: nothing to reclaim yet.
	got, err := s.Observe(t.Context(), cfg, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %s, want no shortfall", got)
	}

	// One more GiB asked for, and the floor no longer fits.
	r.Published("team/other", 1<<30)
	got, err = s.Observe(t.Context(), cfg, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1<<30 {
		t.Errorf("got %s, want 1GiB", got)
	}
}

func TestASourceNamesItselfForTheReclaimThatFollows(t *testing.T) {
	// A reclaim says what drove it, and "volume requests" is the difference
	// between an operator looking at pods and looking at claims.
	if got := volumes.NewSource(volumes.NewRegistry()).Name(); got != "volume requests" {
		t.Errorf("called itself %q", got)
	}
}

// serve starts the reporting surface and returns a client for it.
func serve(t *testing.T, r *volumes.Registry) (*volumes.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(volumes.Handler(r, "secret"))
	t.Cleanup(srv.Close)
	return volumes.NewClient(srv.URL, "secret", 5*time.Second), srv
}

func TestAReportTravelsFromThePluginToTheWatch(t *testing.T) {
	r := volumes.NewRegistry()
	c, _ := serve(t, r)

	if err := c.Published(t.Context(), "team/imagenet", 12<<20); err != nil {
		t.Fatal(err)
	}
	count, bytes := r.Requested()
	if count != 1 || bytes != 12<<20 {
		t.Fatalf("got %d volumes of %d bytes", count, bytes)
	}

	if err := c.Unpublished(t.Context(), "team/imagenet"); err != nil {
		t.Fatal(err)
	}
	if count, _ := r.Requested(); count != 0 {
		t.Errorf("%d volumes left", count)
	}
}

func TestAVolumeIdWithASlashSurvivesTheTrip(t *testing.T) {
	// A volume is named namespace/name, so the id has a slash in it and the
	// route has to carry the whole of it. A route that stopped at the first
	// segment would forget a different volume than the one that went away.
	r := volumes.NewRegistry()
	c, _ := serve(t, r)
	r.Published("team/imagenet", 1<<20)
	if err := c.Unpublished(t.Context(), "team/imagenet"); err != nil {
		t.Fatal(err)
	}
	if count, _ := r.Requested(); count != 0 {
		t.Errorf("%d volumes left, so the id did not survive the route", count)
	}
}

func TestReportingWithoutTheTokenIsRefused(t *testing.T) {
	// What arrives here raises the shortfall the node reclaims against, so
	// anything that could post to it could make a node throw its cache away.
	r := volumes.NewRegistry()
	_, srv := serve(t, r)
	wrong := volumes.NewClient(srv.URL, "not-the-token", 5*time.Second)
	if err := wrong.Published(t.Context(), "team/imagenet", 1<<20); err == nil {
		t.Fatal("an unauthorised report was accepted")
	}
	if count, _ := r.Requested(); count != 0 {
		t.Errorf("%d volumes were recorded anyway", count)
	}
}

func TestAReportWithNoVolumeIsRefused(t *testing.T) {
	r := volumes.NewRegistry()
	_, srv := serve(t, r)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/volumes", strings.NewReader(`{"bytes":5}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %s", resp.Status)
	}
}

func TestUnpublishingSomethingUnknownIsNotAnError(t *testing.T) {
	// The plugin repeats an unpublish it did not hear the answer to, and the
	// agent may have restarted since the publish. Both are asking for a state
	// that already holds.
	r := volumes.NewRegistry()
	c, _ := serve(t, r)
	if err := c.Unpublished(t.Context(), "team/never-seen"); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestAnOperatorCanSeeWhatTheNodeWasAskedFor(t *testing.T) {
	r := volumes.NewRegistry()
	r.Published("team/imagenet", 3<<20)
	_, srv := serve(t, r)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/volumes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		Volumes int   `json:"volumes"`
		Bytes   int64 `json:"bytes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Volumes != 1 || got.Bytes != 3<<20 {
		t.Errorf("got %+v", got)
	}
}

func TestAVolumeIdIsNotPartOfTheURL(t *testing.T) {
	// The id comes from a volume's handle, which is whatever was written into
	// the object. A question mark in it would make the rest of the id a query
	// string, and the volume that went away would not be the one forgotten.
	r := volumes.NewRegistry()
	c, _ := serve(t, r)
	for _, id := range []string{
		"team/imagenet",
		"team/im age",
		"team/what?next=1",
		"team/a#b",
		"team/sub/deep",
	} {
		r.Published(id, 1<<20)
		if err := c.Unpublished(t.Context(), id); err != nil {
			t.Errorf("%q: %v", id, err)
			continue
		}
		if count, _ := r.Requested(); count != 0 {
			t.Errorf("%q did not survive the round trip, %d left", id, count)
			r.Unpublished(id)
		}
	}
}
