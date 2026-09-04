package checkpoint

import (
	"errors"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/internal/lease"
)

// TestTheDefaultCannotLoseWork is the whole safety argument: a user who has
// not chosen gets the acknowledgement that survives a node.
func TestTheDefaultCannotLoseWork(t *testing.T) {
	if got := Ack("").WithDefault(); got != Durable {
		t.Errorf("the default acknowledgement is %q, want %q", got, Durable)
	}
	if got := Ack(Staged).WithDefault(); got != Staged {
		t.Errorf("a stated policy was overridden: %q", got)
	}
}

// TestEachAcknowledgementNamesItsOwnFailure covers the confusion this document
// exists to prevent: a word that means whichever state the reader hoped for.
func TestEachAcknowledgementNamesItsOwnFailure(t *testing.T) {
	survives, lost, err := Staged.Survives()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lost, "node") {
		t.Errorf("staged does not say it is lost with the node: %q", lost)
	}
	if !strings.Contains(survives, "restart") {
		t.Errorf("staged does not say what it survives: %q", survives)
	}
	if _, _, err := Durable.Survives(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ack("committed").Survives(); err == nil {
		t.Error("a word outside the vocabulary was accepted, which is the confusion this prevents")
	}
}

// TestStagingRefusesRevocableCapacity is the rule that keeps a reclaim from
// taking the only copy of a job's progress.
func TestStagingRefusesRevocableCapacity(t *testing.T) {
	for _, c := range []lease.Class{lease.Opportunistic, lease.Elastic, lease.Guaranteed} {
		err := Check(Reservation{Bytes: 1 << 30, Class: c}, 8<<30)
		switch c {
		case lease.Guaranteed:
			if err != nil {
				t.Errorf("guaranteed capacity was refused: %v", err)
			}
		default:
			if !errors.Is(err, ErrRevocable) {
				t.Errorf("%s capacity was accepted for staging: %v", c, err)
			}
			if err != nil && !strings.Contains(err.Error(), "only copy") {
				t.Errorf("the refusal does not say why: %v", err)
			}
		}
	}
}

// TestACheckpointLargerThanTheShareIsRefused covers the cap RFC-0005 puts on
// guaranteed capacity: a node cannot promise all of itself to staging.
func TestACheckpointLargerThanTheShareIsRefused(t *testing.T) {
	err := Check(Reservation{Bytes: 16 << 30, Class: lease.Guaranteed}, 8<<30)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// Refused whole rather than reserved in part: half a staged checkpoint
	// still has to be written through, having wasted the wait.
	for _, want := range []string{"16.00GiB", "8.00GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if err := Check(Reservation{Bytes: 8 << 30, Class: lease.Guaranteed}, 8<<30); err != nil {
		t.Errorf("a checkpoint exactly filling the share was refused: %v", err)
	}
}

func TestAReservationMustReserveSomething(t *testing.T) {
	if err := Check(Reservation{Class: lease.Guaranteed}, 1<<30); err == nil {
		t.Error("a reservation of nothing was accepted")
	}
}
