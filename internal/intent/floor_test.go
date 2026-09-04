package intent

import "testing"

// TestTheFloorRaisesAndNeverLowers is the asymmetry the whole design rests on:
// an administrator may strengthen what a user asked for, and a user who asked
// for more keeps it.
func TestTheFloorRaisesAndNeverLowers(t *testing.T) {
	f := Floor{Durability: DurabilityReplicated}

	for _, c := range []struct {
		asked, want Durability
	}{
		{DurabilityNone, DurabilityReplicated},
		{DurabilityBackend, DurabilityReplicated},
		{DurabilityReplicated, DurabilityReplicated},
		{DurabilityRackTolerant, DurabilityRackTolerant},
	} {
		got := f.Apply(Intent{Durability: c.asked}).Durability
		if got != c.want {
			t.Errorf("a user asking for %q under a %q floor got %q, want %q",
				c.asked, f.Durability, got, c.want)
		}
	}
}

// TestAUserWhoDeclaredNothingIsHeldToTheFloor matters because the zero value is
// the default intent, and a floor that skipped it would apply to everyone
// except the users least likely to have thought about durability.
func TestAUserWhoDeclaredNothingIsHeldToTheFloor(t *testing.T) {
	got := Floor{Durability: DurabilityRackTolerant}.Apply(Intent{})
	if got.Durability != DurabilityRackTolerant {
		t.Errorf("an undeclared intent under a rack-tolerant floor got %q", got.Durability)
	}
	// The other two words are the user's, and a floor does not touch them.
	if got.Latency != LatencyBestEffort || got.Cost != CostBalanced {
		t.Errorf("the floor changed latency or cost: %+v", got)
	}
}

// TestNoFloorChangesNothing keeps an unset floor from being read as a floor of
// none, which would be a ceiling on anyone who declared nothing.
func TestNoFloorChangesNothing(t *testing.T) {
	asked := Intent{Durability: DurabilityReplicated, Latency: LatencyCached, Cost: CostCheapest}
	if got := (Floor{}).Apply(asked); got != asked {
		t.Errorf("an unset floor changed %+v into %+v", asked, got)
	}
}

// TestAFloorNamingNothingRealIsRefused covers the misconfiguration that would
// otherwise leave an administrator believing a requirement was in force: an
// unknown word ranks zero and would quietly raise nothing.
func TestAFloorNamingNothingRealIsRefused(t *testing.T) {
	if err := (Floor{Durability: "very-durable"}).Validate(); err == nil {
		t.Error("a floor naming a durability that does not exist was accepted")
	}
	if err := (Floor{}).Validate(); err != nil {
		t.Errorf("an unset floor was refused: %v", err)
	}
	if err := (Floor{Durability: DurabilityNone}).Validate(); err != nil {
		t.Errorf("a floor of none was refused: %v", err)
	}
}

// TestAFloorDoesNotCorrectATypo matters because raising a word this project
// does not publish would replace it with a valid one, and the user's mistake
// would be silently corrected rather than reported.
func TestAFloorDoesNotCorrectATypo(t *testing.T) {
	got := Floor{Durability: DurabilityRackTolerant}.Apply(Intent{Durability: "replicted"})
	if got.Durability != "replicted" {
		t.Errorf("a misspelled durability was raised to %q instead of being left to fail validation", got.Durability)
	}
	if err := got.Validate(); err == nil {
		t.Error("the misspelling was no longer reported")
	}
}
