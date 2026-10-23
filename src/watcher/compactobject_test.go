package watcher

import (
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testPod(name, ns, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"namespace":       ns,
			"uid":             "uid-" + name,
			"resourceVersion": rv,
			"labels":          map[string]any{"app": "demo"},
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{...}",
				"keep": "me",
			},
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"ratio":    1.0,
			"half":     0.5,
			"name":     "main",
			"containers": []any{
				map[string]any{"name": "c1", "image": "nginx", "ports": []any{map[string]any{"containerPort": int64(80)}}},
			},
		},
	}}
}

func TestTransformObject_StripsStampsAndRoundTrips(t *testing.T) {
	w := &watcher{logger: discardLogger()}
	transform := w.transformObject("v1", "Pod")

	out, err := transform(testPod("p1", "ns1", "7"))
	require.NoError(t, err)
	compact, ok := out.(*compactObject)
	require.True(t, ok, "expected *compactObject, got %T", out)

	u, err := compact.decode()
	require.NoError(t, err)

	assert.Equal(t, "v1", u.GetAPIVersion())
	assert.Equal(t, "Pod", u.GetKind())
	assert.Empty(t, u.GetManagedFields())
	_, hasManaged, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "managedFields")
	assert.False(t, hasManaged)
	assert.Equal(t, map[string]string{"keep": "me"}, u.GetAnnotations())

	// Everything else equal; the integral float becomes int64.
	want := testPod("p1", "ns1", "7")
	want.SetAPIVersion("v1")
	want.SetKind("Pod")
	want.SetManagedFields(nil)
	want.SetAnnotations(map[string]string{"keep": "me"})
	require.NoError(t, unstructured.SetNestedField(want.Object, int64(1), "spec", "ratio"))
	assert.Equal(t, want.Object, u.Object)

	ratio, found, err := unstructured.NestedInt64(u.Object, "spec", "ratio")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(1), ratio)
	replicas, _, err := unstructured.NestedInt64(u.Object, "spec", "replicas")
	require.NoError(t, err)
	assert.Equal(t, int64(3), replicas)
}

func TestTransformObject_KeepsExistingTypeMeta(t *testing.T) {
	w := &watcher{logger: discardLogger()}
	p := testPod("p1", "ns1", "7")
	p.SetAPIVersion("apps/v1")
	p.SetKind("Other")
	out, err := w.transformObject("v1", "Pod")(p)
	require.NoError(t, err)
	u, err := out.(*compactObject).decode()
	require.NoError(t, err)
	assert.Equal(t, "apps/v1", u.GetAPIVersion())
	assert.Equal(t, "Other", u.GetKind())
}

func TestTransformObject_IdempotentAndPassthrough(t *testing.T) {
	w := &watcher{logger: discardLogger()}
	transform := w.transformObject("v1", "Pod")

	out, err := transform(testPod("p1", "ns1", "7"))
	require.NoError(t, err)
	again, err := transform(out)
	require.NoError(t, err)
	assert.Same(t, out, again)

	type other struct{ x int }
	o := &other{1}
	got, err := transform(o)
	require.NoError(t, err)
	assert.Same(t, o, got)
}

func TestTransformObject_MarshalErrorFallsBackToUnstructured(t *testing.T) {
	w := &watcher{logger: discardLogger()}
	p := testPod("p1", "ns1", "7")
	// chan values cannot be serialized.
	p.Object["spec"] = map[string]any{"bad": make(chan int)}
	out, err := w.transformObject("v1", "Pod")(p)
	require.NoError(t, err)
	assert.Same(t, p, out)
	assert.Empty(t, p.GetManagedFields())
}

func TestCompactObject_KeysAndAccessor(t *testing.T) {
	c, err := newCompactObject(func() *unstructured.Unstructured {
		p := testPod("p1", "ns1", "42")
		p.SetAPIVersion("v1")
		p.SetKind("Pod")
		return p
	}())
	require.NoError(t, err)

	key, err := cache.MetaNamespaceKeyFunc(c)
	require.NoError(t, err)
	assert.Equal(t, "ns1/p1", key)

	tomb := cache.DeletedFinalStateUnknown{Key: "ns1/p1", Obj: c}
	key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(tomb)
	require.NoError(t, err)
	assert.Equal(t, "ns1/p1", key)

	acc, err := meta.Accessor(c)
	require.NoError(t, err)
	assert.Equal(t, "42", acc.GetResourceVersion())
	assert.Equal(t, "uid-p1", string(acc.GetUID()))

	cp := c.DeepCopyObject().(*compactObject)
	assert.NotSame(t, c, cp)
	assert.Equal(t, c.Name, cp.Name)
	assert.Equal(t, c.raw, cp.raw)
	assert.True(t, c.GetObjectKind().GroupVersionKind().Empty())
}

func TestObjectToUnstructured(t *testing.T) {
	p := testPod("p1", "ns1", "1")
	p.SetAPIVersion("v1")
	p.SetKind("Pod")
	c, err := newCompactObject(p)
	require.NoError(t, err)

	u, err := objectToUnstructured(c)
	require.NoError(t, err)
	assert.Equal(t, "p1", u.GetName())

	u, err = objectToUnstructured(p)
	require.NoError(t, err)
	assert.Same(t, p, u)

	u, err = objectToUnstructured(cache.DeletedFinalStateUnknown{Key: "ns1/p1", Obj: c})
	require.NoError(t, err)
	assert.Equal(t, "p1", u.GetName())

	u, err = objectToUnstructured(cache.DeletedFinalStateUnknown{Key: "ns1/p1", Obj: p})
	require.NoError(t, err)
	assert.Same(t, p, u)

	_, err = objectToUnstructured("nope")
	assert.Error(t, err)
	_, err = objectToUnstructured(cache.DeletedFinalStateUnknown{Key: "k", Obj: 5})
	assert.Error(t, err)
}

// BenchmarkRetainedHeap reports the retained heap per object for the nested
// map form versus the compact JSON form. Informative only.
func BenchmarkRetainedHeap(b *testing.B) {
	const n = 2000
	build := func(i int) *unstructured.Unstructured {
		p := testPod("pod-"+string(rune('a'+i%26))+"-"+strconv.Itoa(i), "ns"+strconv.Itoa(i%20), strconv.Itoa(i))
		p.SetAPIVersion("v1")
		p.SetKind("Pod")
		p.SetManagedFields(nil)
		return p
	}
	measure := func(mk func() any) float64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		keep := mk()
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(keep)
		return float64(after.HeapAlloc-before.HeapAlloc) / n
	}
	for i := 0; i < b.N; i++ {
		un := measure(func() any {
			out := make([]*unstructured.Unstructured, n)
			for j := range out {
				out[j] = build(j)
			}
			return out
		})
		co := measure(func() any {
			out := make([]*compactObject, n)
			for j := range out {
				c, err := newCompactObject(build(j))
				if err != nil {
					b.Fatal(err)
				}
				out[j] = c
			}
			return out
		})
		b.ReportMetric(un, "unstructured-B/obj")
		b.ReportMetric(co, "compact-B/obj")
	}
}
