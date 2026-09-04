package kube

import (
	"context"
	"errors"
	"fmt"
)

// DatasetResource names the kind a user declares.
var DatasetResource = Resource{Group: "forebay.io", Version: "v1alpha1", Plural: "datasets"}

// Dataset is what a user asks for: an object in a durable store, and the
// intent attached to it.
//
// A declaration and never a measurement, which is the rule RFC-0014 sets for
// what belongs in etcd. What the system observes about a dataset goes in its
// status, which is the control plane's answer rather than the user's request.
type Dataset struct {
	Metadata Metadata       `json:"metadata"`
	Spec     DatasetSpec    `json:"spec"`
	Status   *DatasetStatus `json:"status,omitempty"`
}

// Metadata is the part of an object's identity this project reads.
type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Generation counts spec changes, so a status can say which spec it
	// describes and a stale one is visible rather than assumed current.
	Generation int64 `json:"generation,omitempty"`
}

// DatasetSpec is the declaration.
type DatasetSpec struct {
	// Object names it in the durable store. The store itself is the
	// controller's configuration rather than the user's, so a dataset does not
	// carry credentials or an endpoint.
	Object string `json:"object"`
}

// DatasetStatus is what the control plane observed.
type DatasetStatus struct {
	// Present says the store has it. Absent is a legitimate state rather than
	// an error: a dataset may be declared before it is uploaded.
	Present bool `json:"present"`
	// Bytes is its size, and is only meaningful when present.
	Bytes int64 `json:"bytes,omitempty"`
	// Reason carries the store's own words when it could not be checked, so
	// an operator is not left to guess between absent and unreachable.
	Reason string `json:"reason,omitempty"`
	// ObservedGeneration is the spec this describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// DatasetList is what the API server returns for a collection.
type DatasetList struct {
	Items []Dataset `json:"items"`
}

// Sizer answers how large an object is, which is the whole of what resolving a
// dataset needs from a backend.
type Sizer interface {
	SizeOf(ctx context.Context, object string) (int64, error)
}

// ErrNoObject rejects a dataset that names nothing.
var ErrNoObject = errors.New("kube: dataset names no object")

// Resolve asks the store about one dataset and returns what to record.
//
// An object that is not there and a store that could not be reached are
// different answers and are kept apart: the first is a dataset waiting for its
// data, and the second is an operator's problem.
func Resolve(ctx context.Context, store Sizer, d Dataset) (DatasetStatus, error) {
	if d.Spec.Object == "" {
		return DatasetStatus{}, fmt.Errorf("%w: %s", ErrNoObject, d.Metadata.Name)
	}
	status := DatasetStatus{ObservedGeneration: d.Metadata.Generation}
	size, err := store.SizeOf(ctx, d.Spec.Object)
	if err != nil {
		status.Reason = err.Error()
		return status, nil
	}
	status.Present, status.Bytes = true, size
	return status, nil
}

// Changed reports whether a status says anything the recorded one does not,
// so a controller writes only when there is something to write.
func Changed(was *DatasetStatus, now DatasetStatus) bool {
	if was == nil {
		return true
	}
	return *was != now
}
