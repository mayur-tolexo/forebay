// Package lineage records what produced what, and how each of those claims is
// known.
//
// RFC-0023's position is that a lineage system which overstates what it knows
// is worse than none: it converts an honest guess into a confident wrong
// answer, which is the thing an auditor exists to catch. So every edge carries
// whether Forebay observed it or somebody asserted it, and a query returns
// that account alongside the graph rather than on request.
package lineage

import (
	"errors"
	"fmt"
	"sort"
)

// Kind is what a node is. Four rather than one, because a checkpoint is not
// read the way a dataset is and giving it a dataset's lifecycle would make
// every checkpoint a candidate for the fast tier.
type Kind int

const (
	// Version is one immutable dataset version.
	Version Kind = iota
	// Run is one execution of a job.
	Run
	// Checkpoint is one saved training state.
	Checkpoint
	// Model is one thing somebody decided to ship.
	Model
)

// String names a kind for a message a person reads.
func (k Kind) String() string {
	switch k {
	case Version:
		return "version"
	case Run:
		return "run"
	case Checkpoint:
		return "checkpoint"
	case Model:
		return "model"
	default:
		return "unknown"
	}
}

// Basis says how an edge is known, which is the whole of this design.
type Basis int

const (
	// Declared was asserted by somebody. Promoting a checkpoint to a model is
	// always this: it is a decision rather than an event, and no amount of
	// instrumentation makes a decision observable.
	Declared Basis = iota
	// Observed was seen by Forebay on a path it served.
	Observed
)

// String names a basis for a message a person reads.
func (b Basis) String() string {
	if b == Observed {
		return "observed"
	}
	return "declared"
}

// Relation is what an edge asserts. Three that are checkable beat any number
// that are not.
type Relation int

const (
	// Read is a run reading a version.
	Read Relation = iota
	// Produced is a run producing a checkpoint.
	Produced
	// Promoted is a checkpoint becoming a model.
	Promoted
)

// String names a relation for a message a person reads.
func (r Relation) String() string {
	switch r {
	case Read:
		return "read"
	case Produced:
		return "produced"
	case Promoted:
		return "promoted"
	default:
		return "unknown"
	}
}

// Node is one thing in the graph.
type Node struct {
	ID   string
	Kind Kind
	// Digest is what the content was when it was recorded. It is what makes
	// tampering detectable: immutability enforced above a store cannot bind
	// that store's own owner, so the honest guarantee is that Forebay can tell
	// you something changed rather than stop it.
	Digest string
	// Retrievable says whether the bytes are still there. A version whose
	// bytes were deleted keeps its node, because the historical fact that a
	// run read it does not stop being true.
	Retrievable bool
}

// Edge is one claim, and how it is known.
type Edge struct {
	From, To string
	Relation Relation
	Basis    Basis
}

var (
	// ErrNoSuchNode reports an edge naming something the graph does not hold.
	ErrNoSuchNode = errors.New("lineage: no such node")
	// ErrDuplicate reports a node added twice under one identity.
	ErrDuplicate = errors.New("lineage: node already recorded")
	// ErrWrongKinds reports a relation between kinds it cannot hold between.
	ErrWrongKinds = errors.New("lineage: relation does not hold between those kinds")
	// ErrNotObservable reports a claim recorded as observed that Forebay
	// cannot observe.
	ErrNotObservable = errors.New("lineage: that relation cannot be observed")
)

// allowed says which kinds a relation runs between, so a graph cannot be built
// that asserts something the model has no meaning for.
//
// It is also what makes a cycle impossible rather than merely refused. Read
// runs from a run to a version, produced from a run to a checkpoint, and
// promoted from a checkpoint to a model, and nothing leads back into a run or
// a version. A traversal therefore cannot loop, and a fourth relation that
// closed the graph would have to be added here, where the property is stated.
var allowed = map[Relation][2]Kind{
	Read:     {Run, Version},
	Produced: {Run, Checkpoint},
	Promoted: {Checkpoint, Model},
}

// Graph holds nodes and the claims between them.
//
// Not safe for concurrent use. Nothing writes it from the read path, and a
// lock would be for a caller this package does not have yet.
type Graph struct {
	nodes map[string]Node
	// out is the edges leaving a node, which is the direction claims are made
	// in; ancestry walks it backwards.
	out map[string][]Edge
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{nodes: map[string]Node{}, out: map[string][]Edge{}}
}

// Add records a node.
func (g *Graph) Add(n Node) error {
	if n.ID == "" {
		return fmt.Errorf("lineage: a node needs an identity")
	}
	if _, dup := g.nodes[n.ID]; dup {
		return fmt.Errorf("%w: %s", ErrDuplicate, n.ID)
	}
	g.nodes[n.ID] = n
	return nil
}

// Link records a claim between two nodes.
func (g *Graph) Link(e Edge) error {
	from, ok := g.nodes[e.From]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchNode, e.From)
	}
	to, ok := g.nodes[e.To]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchNode, e.To)
	}

	kinds, known := allowed[e.Relation]
	if !known || from.Kind != kinds[0] || to.Kind != kinds[1] {
		return fmt.Errorf("%w: %s %s %s", ErrWrongKinds, from.Kind, e.Relation, to.Kind)
	}
	// Promotion is a decision rather than an event, so recording one as
	// observed would be claiming Forebay saw something no instrumentation can
	// see. Refused rather than downgraded: a caller that meant to assert it
	// should say so.
	if e.Relation == Promoted && e.Basis == Observed {
		return fmt.Errorf("%w: %s", ErrNotObservable, e.Relation)
	}
	g.out[e.From] = append(g.out[e.From], e)
	return nil
}

// Answer is a lineage query and the account it gives of itself.
//
// The account travels with the graph rather than being available on request,
// because the second call is the one that gets skipped, and that is how the
// misleading version gets built.
type Answer struct {
	// Nodes is everything reachable, ordered by identity so two answers to the
	// same question read the same.
	Nodes []Node
	// Edges is every claim between them, in the same order.
	Edges []Edge
	// Observed and Asserted count the claims by how they are known. A caller
	// saying "this model was trained on these versions" can see on the same
	// answer how much of that is Forebay's word.
	Observed, Asserted int
	// Unretrievable names versions recorded but no longer held, which is a
	// different answer from absent and a more useful one: the model was
	// trained on data that existed and is gone.
	Unretrievable []string
}

// Complete reports whether every claim in the answer was observed. It is not a
// claim of completeness about the world: Forebay sees only what it served, so
// a job that read around it is invisible either way.
func (a Answer) Complete() bool { return a.Asserted == 0 && a.Observed > 0 }

// Ancestry answers what produced a thing.
//
// Which way an edge points depends on what it asserts, and the traversal has
// to respect that rather than pick one direction. A run's ancestors are the
// versions it read, which its own edges point at. A checkpoint's ancestor is
// the run that produced it, which points at the checkpoint. So ancestry
// follows `read` forwards and the other two backwards, and the alternative —
// storing every edge pointing at its ancestor — would make the graph read
// backwards everywhere it is written.
func (g *Graph) Ancestry(id string) (Answer, error) {
	if _, ok := g.nodes[id]; !ok {
		return Answer{}, fmt.Errorf("%w: %s", ErrNoSuchNode, id)
	}

	// Built once per query rather than kept, since edges are added far more
	// often than ancestry is asked for.
	in := map[string][]Edge{}
	for _, edges := range g.out {
		for _, e := range edges {
			in[e.To] = append(in[e.To], e)
		}
	}

	var a Answer
	seen := map[string]bool{id: true}
	stack := []string{id}
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		n := g.nodes[at]
		a.Nodes = append(a.Nodes, n)
		if n.Kind == Version && !n.Retrievable {
			a.Unretrievable = append(a.Unretrievable, n.ID)
		}

		for _, e := range g.ancestorEdges(at, in) {
			a.Edges = append(a.Edges, e)
			if e.Basis == Observed {
				a.Observed++
			} else {
				a.Asserted++
			}
			next := e.From
			if e.Relation == Read {
				next = e.To
			}
			if !seen[next] {
				seen[next] = true
				stack = append(stack, next)
			}
		}
	}

	sort.Slice(a.Nodes, func(i, j int) bool { return a.Nodes[i].ID < a.Nodes[j].ID })
	sort.Slice(a.Edges, func(i, j int) bool {
		if a.Edges[i].From != a.Edges[j].From {
			return a.Edges[i].From < a.Edges[j].From
		}
		return a.Edges[i].To < a.Edges[j].To
	})
	sort.Strings(a.Unretrievable)
	return a, nil
}

// ancestorEdges is the claims that say where a node came from: what it read,
// and what produced or promoted it.
func (g *Graph) ancestorEdges(at string, in map[string][]Edge) []Edge {
	var out []Edge
	for _, e := range g.out[at] {
		if e.Relation == Read {
			out = append(out, e)
		}
	}
	for _, e := range in[at] {
		if e.Relation != Read {
			out = append(out, e)
		}
	}
	return out
}
