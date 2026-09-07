package csi

import (
	"context"

	"github.com/mayur-tolexo/forebay/internal/grpcwire"
	"github.com/mayur-tolexo/forebay/internal/kube"
)

// Datasets resolves a dataset through the Kubernetes API.
//
// It reads and never writes. The controller plugin is not a reconciler: the
// dataset's status is the operator's answer, and a plugin that filled it in
// would be a second writer of a field with one author.
type Datasets struct{ client *kube.Client }

// NewDatasets points a resolver at a cluster.
func NewDatasets(c *kube.Client) *Datasets { return &Datasets{client: c} }

// Resolve reports the object a dataset names and how large it is.
//
// A dataset that exists but whose bytes are not there is refused. Mounting it
// would give a pod an empty directory with no way to tell that from a dataset
// that is genuinely empty, and the pod would read nothing and carry on.
func (d *Datasets) Resolve(ctx context.Context, namespace, name string) (string, int64, error) {
	r := kube.DatasetResource
	r.Namespace = namespace

	var ds kube.Dataset
	if err := d.client.Get(ctx, r, name, &ds); err != nil {
		if kube.NotFound(err) {
			return "", 0, grpcwire.Errorf(grpcwire.NotFound,
				"no dataset %s/%s", namespace, name)
		}
		// Unavailable rather than Internal: the provisioner retries this one,
		// and an API server that is briefly away is the ordinary case.
		return "", 0, grpcwire.Errorf(grpcwire.Unavailable,
			"reading dataset %s/%s: %v", namespace, name, err)
	}
	if ds.Spec.Object == "" {
		return "", 0, grpcwire.Errorf(grpcwire.FailedPrecondition,
			"dataset %s/%s names no object in the store", namespace, name)
	}
	switch {
	case ds.Status == nil:
		// Nothing has reconciled it yet, which is a wait rather than a
		// refusal: the provisioner retries and the operator is on its way.
		return "", 0, grpcwire.Errorf(grpcwire.Unavailable,
			"dataset %s/%s has not been reconciled yet", namespace, name)
	case !ds.Status.Present:
		return "", 0, grpcwire.Errorf(grpcwire.FailedPrecondition,
			"dataset %s/%s is declared but its object %q is not in the store%s",
			namespace, name, ds.Spec.Object, because(ds.Status.Reason))
	}
	return ds.Spec.Object, ds.Status.Bytes, nil
}

// because appends the store's own words when there are any, so an operator is
// not left to guess between absent and unreachable.
func because(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}
