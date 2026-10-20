package watcher

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// compactObject is the form in which watched objects live in the informer
// cache. An *unstructured.Unstructured holds the object as nested
// map[string]any, which costs several times the size of its JSON encoding
// (map headers, interface boxes, string headers per key and value). With every
// discovered kind watched cluster-wide that dominates the operator's heap, and
// the full body is already persisted in Valkey, so the cache only needs enough
// to key, index and diff entries plus the serialized body.
//
// ObjectMeta carries just Name, Namespace, UID and ResourceVersion: client-go's
// MetaNamespaceKeyFunc, the namespace indexer and the resourceVersion
// comparison in the shared informer all go through meta.Accessor. Everything
// else is in raw and is recovered with decode.
//
// Known behaviour change: decode goes through JSON, so a float value that is
// integral (1.0) comes back as int64(1). The JSON written to Valkey and event
// consumers is unaffected, because a float64 1.0 marshals as `1` anyway.
//
// A compactObject is immutable once built; raw is shared between deep copies.
type compactObject struct {
	metav1.ObjectMeta
	raw []byte
}

// newCompactObject serializes u. u must have its kind set (the unstructured
// JSON codec rejects objects without one).
func newCompactObject(u *unstructured.Unstructured) (*compactObject, error) {
	raw, err := u.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return &compactObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:            u.GetName(),
			Namespace:       u.GetNamespace(),
			UID:             u.GetUID(),
			ResourceVersion: u.GetResourceVersion(),
		},
		raw: raw,
	}, nil
}

// GetObjectKind implements runtime.Object. The type information lives inside
// raw, so there is nothing to expose here.
func (c *compactObject) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}

// DeepCopyObject implements runtime.Object. raw is never written after
// construction, so the copy shares it.
func (c *compactObject) DeepCopyObject() runtime.Object {
	if c == nil {
		return nil
	}
	return &compactObject{
		ObjectMeta: *c.ObjectMeta.DeepCopy(),
		raw:        c.raw,
	}
}

// decode builds a fresh *unstructured.Unstructured from raw. It uses the
// unstructured JSON codec (not encoding/json into a map) so integers come back
// as int64 like they do from the API server.
func (c *compactObject) decode() (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(c.raw); err != nil {
		return nil, err
	}
	return u, nil
}

// objectToUnstructured converts anything the informer store or an event
// handler can hand us into an *unstructured.Unstructured. It accepts
// *compactObject (decoded), *unstructured.Unstructured (as-is: the store can
// hold both because the transform falls back to the unstructured object when
// serialization fails) and cache.DeletedFinalStateUnknown wrapping either.
func objectToUnstructured(obj any) (*unstructured.Unstructured, error) {
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		return o, nil
	case *compactObject:
		return o.decode()
	case cache.DeletedFinalStateUnknown:
		return objectToUnstructured(o.Obj)
	default:
		return nil, fmt.Errorf("unsupported object type %T", obj)
	}
}

// sameObject reports whether a and b are the identical cached pointer, which
// is what the informer passes to UpdateFunc on resync.
func sameObject(a, b any) bool {
	switch x := a.(type) {
	case *compactObject:
		y, ok := b.(*compactObject)
		return ok && x == y
	case *unstructured.Unstructured:
		y, ok := b.(*unstructured.Unstructured)
		return ok && x == y
	}
	return false
}
