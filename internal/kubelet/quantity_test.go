package kubelet_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/kubelet"
)

func TestQuantitiesTheClusterActuallyUses(t *testing.T) {
	// The right-hand column is what a real API server stored each of these
	// as, checked against it rather than worked out from the docs. Reading
	// one wrongly reclaims somebody's cache.
	for in, want := range map[string]int64{
		"2Gi": 2 << 30, "10Gi": 10 << 30, "50Gi": 50 << 30,
		"512Mi": 512 << 20, "1Ki": 1024, "1Ti": 1 << 40,
		"1G": 1e9, "500M": 5e8, "1k": 1000,
		"1073741824": 1 << 30, "0": 0, "2048": 2048,

		// Stored as 1536Mi and 512Mi: a fraction of a binary suffix is
		// ordinary, and refusing it would drop the pod.
		"1.5Gi": 1536 << 20, "0.5Gi": 512 << 20,

		// Exponents. The server keeps 1E3 and 1E apart, so E before digits
		// is an exponent and E ending the string is exa.
		"1e3": 1000, "1E3": 1000, "2.5e3": 2500, "1E": 1e18, "1Ei": 1 << 60,

		// Sub-unit suffixes, which is how the server canonicalises anything
		// that is not a whole number of bytes. 1023.9Mi becomes
		// 1073636966400m, and 1.1Ki becomes 1126400m.
		"1500m": 2, "1073636966400m": 1073636967, "1.1Ki": 1127, "10n": 1,
	} {
		got, err := kubelet.ParseQuantity(in)
		if err != nil {
			t.Errorf("ParseQuantity(%q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseQuantity(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAFractionOfAByteRoundsUp(t *testing.T) {
	// A request is a claim on space. Rounding it down is the direction that
	// under-reclaims, which is the direction that hurts.
	for in, want := range map[string]int64{"1m": 1, "1500m": 2, "2000m": 2, "2001m": 3} {
		got, err := kubelet.ParseQuantity(in)
		if err != nil {
			t.Fatalf("ParseQuantity(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseQuantity(%q) = %d, want %d rounded up", in, got, want)
		}
	}
}

func TestAQuantityThisCannotReadIsRefused(t *testing.T) {
	// Refused rather than approximated: a request nobody can read is not
	// evidence about how much a pod will write. "1/3" is the case that makes
	// this worth a test, since the number parser underneath accepts a ratio
	// the API server never would.
	for _, in := range []string{"", "  ", "-1Gi", "-5", "Gi", "12Xi", "one", "1/3", "0x10", "9223372036854775807Gi"} {
		if got, err := kubelet.ParseQuantity(in); err == nil {
			t.Errorf("ParseQuantity(%q) = %d, want a refusal", in, got)
		}
	}
}

func TestAnEnormousExponentIsRefusedWithoutExpandingIt(t *testing.T) {
	// The API server stores "1e1000000" as "10e999999", so this arrives on a
	// real node. Expanding it costs a million digits of arithmetic before the
	// answer can be rejected, and the watch would pay that every pass for as
	// long as the pod exists.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if got, err := kubelet.ParseQuantity("10e999999"); err == nil {
			t.Errorf("ParseQuantity = %d, want a refusal", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parsing an enormous exponent did not return promptly")
	}
}

func TestARefusalNamesTheQuantityAsWritten(t *testing.T) {
	// The operator is looking at a pod spec. An error naming the number with
	// its suffix stripped sends them looking for a field that does not exist.
	_, err := kubelet.ParseQuantity("1.2.3Gi")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "1.2.3Gi") {
		t.Errorf("error does not name the quantity as written: %v", err)
	}
}

func TestAnEnormousMantissaIsRefusedWithoutExpandingIt(t *testing.T) {
	// The exponent is not the only way to ask for a lot of arithmetic. A real
	// API server stored a hundred thousand digits of this verbatim rather
	// than clamping them, and the watch would parse it once per pass for as
	// long as the pod lived.
	//
	// The refusal has to be for the length. Rejecting it after the expansion
	// looks identical from the outside, and a million digits still returns in
	// under a second, so a test that only timed it would pass either way.
	start := time.Now()
	_, err := kubelet.ParseQuantity(strings.Repeat("9", 1000000))
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "characters long") {
		t.Errorf("refused after doing the work rather than for its length: %v", err)
	}
	// Generous against the 780ms the expansion costs, tight enough that doing
	// the work would show.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("refusing took %s, which is long enough to have parsed it", elapsed)
	}
}

func TestTheLongestRealQuantityStillParses(t *testing.T) {
	// The bound has to clear what a real API server stores. The longest of
	// those seen is a clamped byte count and a milli-suffixed fraction.
	for _, in := range []string{"9223372036854775807", "1073636966400m"} {
		if _, err := kubelet.ParseQuantity(in); err != nil {
			t.Errorf("ParseQuantity(%q) = %v, want it inside the length bound", in, err)
		}
	}
}
