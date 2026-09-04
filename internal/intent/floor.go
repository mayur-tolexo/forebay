package intent

import "fmt"

// Floor is what an administrator requires of every intent in their scope.
//
// It carries durability and nothing else, which is the whole of the decision.
// An administrator may raise what a dataset requires of its backend, because
// that is a safety property their organisation is entitled to set. They may not
// raise latency or cost, because those two words authorise Forebay to spend
// borrowed capacity, and an administrator raising them would commit a user to
// spending that the user declined.
//
// There is no ceiling. A ceiling would let an administrator silently serve a
// user less durability than they declared, which is the failure a declarative
// interface exists to prevent. A tenant who must be limited is limited by quota
// and by refusal, both of which the user can see.
type Floor struct {
	Durability Durability
}

// durabilityRank orders the durability words by how much they require, which
// is what makes strengthening a comparison rather than a policy table.
var durabilityRank = map[Durability]int{
	DurabilityNone:         0,
	DurabilityBackend:      1,
	DurabilityReplicated:   2,
	DurabilityRackTolerant: 3,
}

// Apply raises an intent to the floor, and never lowers it.
//
// Defaults are filled first, so a user who declared nothing is held to the
// floor rather than to the zero value. A user who asked for more keeps it.
func (f Floor) Apply(i Intent) Intent {
	i = i.WithDefaults()
	// A word this project does not publish is left alone rather than raised.
	// Raising it would replace it with something valid, and the user's typo
	// would be corrected into silence instead of reported by Validate.
	asked, known := durabilityRank[i.Durability]
	if !known {
		return i
	}
	if durabilityRank[f.Durability] > asked {
		i.Durability = f.Durability
	}
	return i
}

// Validate refuses a floor naming a durability that does not exist, since one
// that is silently ignored is worse than one that is refused: an administrator
// would believe a requirement was in force.
func (f Floor) Validate() error {
	if f.Durability == "" {
		return nil
	}
	if _, ok := durabilityRank[f.Durability]; !ok {
		return fmt.Errorf("intent: floor names unknown durability %q", f.Durability)
	}
	return nil
}
