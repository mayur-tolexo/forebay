package lineage

import (
	"errors"
	"testing"
)

// trained builds the graph a real answer comes from: two dataset versions read
// by a run, which produced a checkpoint, which somebody promoted to a model.
func trained(t *testing.T) *Graph {
	t.Helper()
	g := New()
	for _, n := range []Node{
		{ID: "imagenet/v17", Kind: Version, Digest: "sha:aaa", Retrievable: true},
		{ID: "captions/v3", Kind: Version, Digest: "sha:bbb", Retrievable: true},
		{ID: "run-91", Kind: Run},
		{ID: "ckpt-4400", Kind: Checkpoint, Digest: "sha:ccc"},
		{ID: "vision-2", Kind: Model},
	} {
		if err := g.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []Edge{
		{From: "run-91", To: "imagenet/v17", Relation: Read, Basis: Observed},
		{From: "run-91", To: "captions/v3", Relation: Read, Basis: Observed},
		{From: "run-91", To: "ckpt-4400", Relation: Produced, Basis: Observed},
		{From: "ckpt-4400", To: "vision-2", Relation: Promoted, Basis: Declared},
	} {
		if err := g.Link(e); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestAncestryReachesTheDataThroughTheRun is the question the whole document
// exists to answer: six months on, which data produced this model.
func TestAncestryReachesTheDataThroughTheRun(t *testing.T) {
	got, err := trained(t).Ancestry("vision-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 5 {
		t.Errorf("reached %d nodes, want the whole chain of 5: %+v", len(got.Nodes), got.Nodes)
	}
	var versions int
	for _, n := range got.Nodes {
		if n.Kind == Version {
			versions++
		}
	}
	if versions != 2 {
		t.Errorf("reached %d versions, want both", versions)
	}
}

// TestAnAnswerAccountsForItself is the design. A caller saying "this model was
// trained on these versions" must be able to see, on the same answer, how much
// of that is Forebay's word and how much is somebody else's.
func TestAnAnswerAccountsForItself(t *testing.T) {
	got, err := trained(t).Ancestry("vision-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Observed != 3 {
		t.Errorf("counted %d observed claims, want the three Forebay saw", got.Observed)
	}
	if got.Asserted != 1 {
		t.Errorf("counted %d asserted claims, want the promotion", got.Asserted)
	}
	if got.Complete() {
		t.Error("an answer containing an assertion reported itself as wholly observed")
	}

	// The run alone is reached by observation only.
	fromRun, err := trained(t).Ancestry("ckpt-4400")
	if err != nil {
		t.Fatal(err)
	}
	if !fromRun.Complete() {
		t.Errorf("an answer with only observed claims reported %d asserted", fromRun.Asserted)
	}
}

// TestAnEmptyAnswerIsNotComplete keeps a node nothing points at from reporting
// itself as wholly observed, which would read as a confident nothing.
func TestAnEmptyAnswerIsNotComplete(t *testing.T) {
	g := New()
	if err := g.Add(Node{ID: "orphan", Kind: Model}); err != nil {
		t.Fatal(err)
	}
	got, err := g.Ancestry("orphan")
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete() {
		t.Error("a node with no ancestry reported itself as wholly observed")
	}
}

// TestDeletedDataIsRecordedAndNotRetrievable covers the answer that is more
// useful than absent: the model was trained on data that existed and is gone.
func TestDeletedDataIsRecordedAndNotRetrievable(t *testing.T) {
	g := trained(t)
	// The bytes went; the historical fact that a run read them did not.
	n := g.nodes["captions/v3"]
	n.Retrievable = false
	g.nodes["captions/v3"] = n

	got, err := g.Ancestry("vision-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unretrievable) != 1 || got.Unretrievable[0] != "captions/v3" {
		t.Errorf("unretrievable = %v, want the deleted version", got.Unretrievable)
	}
	// Still in the graph, with its digest, so the record is not broken.
	var found bool
	for _, node := range got.Nodes {
		if node.ID == "captions/v3" && node.Digest == "sha:bbb" {
			found = true
		}
	}
	if !found {
		t.Error("a deleted version lost its node, so the graph no longer records what was read")
	}
}

// TestAPromotionCannotBeObserved matters because promoting a checkpoint to a
// model is a decision rather than an event, and recording one as observed
// would claim Forebay saw something no instrumentation can see.
func TestAPromotionCannotBeObserved(t *testing.T) {
	g := New()
	for _, n := range []Node{{ID: "c", Kind: Checkpoint}, {ID: "m", Kind: Model}} {
		if err := g.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	err := g.Link(Edge{From: "c", To: "m", Relation: Promoted, Basis: Observed})
	if !errors.Is(err, ErrNotObservable) {
		t.Errorf("an observed promotion gave %v", err)
	}
	if err := g.Link(Edge{From: "c", To: "m", Relation: Promoted, Basis: Declared}); err != nil {
		t.Errorf("a declared promotion was refused: %v", err)
	}
}

// TestARelationMustHoldBetweenItsKinds keeps a graph from asserting something
// the model has no meaning for.
func TestARelationMustHoldBetweenItsKinds(t *testing.T) {
	g := trained(t)
	for _, c := range []struct {
		name string
		edge Edge
	}{
		{"a version reading a run", Edge{From: "imagenet/v17", To: "run-91", Relation: Read}},
		{"a run promoting a model", Edge{From: "run-91", To: "vision-2", Relation: Promoted}},
		{"a run producing a version", Edge{From: "run-91", To: "imagenet/v17", Relation: Produced}},
		{"a relation that does not exist", Edge{From: "run-91", To: "ckpt-4400", Relation: Relation(9)}},
	} {
		if err := g.Link(c.edge); !errors.Is(err, ErrWrongKinds) {
			t.Errorf("%s gave %v", c.name, err)
		}
	}
}

// TestTheKindRulesCannotCycle is stronger than refusing a cycle when one is
// asserted: nothing leads back into a run or a version, so a traversal cannot
// loop and there is no check to forget. A fourth relation that closed the
// graph would have to be added to the same table this walks, which is what
// makes the property maintainable.
func TestTheKindRulesCannotCycle(t *testing.T) {
	edges := map[Kind][]Kind{}
	for _, k := range allowed {
		edges[k[0]] = append(edges[k[0]], k[1])
	}

	// Depth-first from every kind, refusing to revisit anything on the path.
	var walk func(at Kind, path map[Kind]bool) bool
	walk = func(at Kind, path map[Kind]bool) bool {
		if path[at] {
			return true
		}
		path[at] = true
		for _, next := range edges[at] {
			if walk(next, path) {
				return true
			}
		}
		delete(path, at)
		return false
	}
	for _, k := range []Kind{Version, Run, Checkpoint, Model} {
		if walk(k, map[Kind]bool{}) {
			t.Errorf("the relations let a %s reach itself, so a traversal can loop", k)
		}
	}
}

// TestAnEdgeNeedsBothItsNodes keeps a claim about something the graph does not
// hold from being recorded as if it did.
func TestAnEdgeNeedsBothItsNodes(t *testing.T) {
	g := trained(t)
	if err := g.Link(Edge{From: "run-92", To: "imagenet/v17", Relation: Read}); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("an edge from a node that does not exist gave %v", err)
	}
	if err := g.Link(Edge{From: "run-91", To: "imagenet/v99", Relation: Read}); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("an edge to a node that does not exist gave %v", err)
	}
	if _, err := g.Ancestry("nothing"); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("ancestry of a node that does not exist gave %v", err)
	}
}

// TestANodeIsRecordedOnce keeps a second version of one identity from
// silently replacing the first, digest and all.
func TestANodeIsRecordedOnce(t *testing.T) {
	g := trained(t)
	if err := g.Add(Node{ID: "run-91", Kind: Run}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("re-adding a node gave %v", err)
	}
	if err := g.Add(Node{Kind: Run}); err == nil {
		t.Error("a node with no identity was accepted")
	}
}

// TestAnAnswerReadsTheSameTwice matters because two answers to one question
// that differ only in order look like two different answers.
func TestAnAnswerReadsTheSameTwice(t *testing.T) {
	first, err := trained(t).Ancestry("vision-2")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := trained(t).Ancestry("vision-2")
		if err != nil {
			t.Fatal(err)
		}
		for j := range got.Nodes {
			if got.Nodes[j] != first.Nodes[j] {
				t.Fatalf("node %d differs between answers: %+v against %+v", j, got.Nodes[j], first.Nodes[j])
			}
		}
		for j := range got.Edges {
			if got.Edges[j] != first.Edges[j] {
				t.Fatalf("edge %d differs between answers: %+v against %+v", j, got.Edges[j], first.Edges[j])
			}
		}
	}
}

// TestNamesAreReadable covers the strings that end up in a message a person
// reads, since an unknown kind printed as a number helps nobody.
func TestNamesAreReadable(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Version.String(), "version"},
		{Run.String(), "run"},
		{Checkpoint.String(), "checkpoint"},
		{Model.String(), "model"},
		{Kind(9).String(), "unknown"},
		{Observed.String(), "observed"},
		{Declared.String(), "declared"},
		{Read.String(), "read"},
		{Produced.String(), "produced"},
		{Promoted.String(), "promoted"},
		{Relation(9).String(), "unknown"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
