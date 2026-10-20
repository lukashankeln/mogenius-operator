package watcher

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"mogenius-operator/src/utils"
)

// newRoutingTestWatcher builds a watcher with two distinct factories so we can
// assert that factoryForGVR routes Secrets to the dedicated (field-selector)
// factory and everything else to the shared one.
func newRoutingTestWatcher() (*watcher, dynamicinformer.DynamicSharedInformerFactory, dynamicinformer.DynamicSharedInformerFactory) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	shared := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, time.Minute, metav1.NamespaceAll, nil)
	secret := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, time.Minute, metav1.NamespaceAll, func(opts *metav1.ListOptions) {
		opts.FieldSelector = secretWatchFieldSelector
	})
	return &watcher{factory: shared, secretFactory: secret}, shared, secret
}

func TestFactoryForGVR_RoutesSecretsToSecretFactory(t *testing.T) {
	w, _, secret := newRoutingTestWatcher()

	secretsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	assert.Same(t, secret, w.factoryForGVR(secretsGVR),
		"core/v1 Secret must route to the dedicated secretFactory")
}

func TestFactoryForGVR_RoutesOtherResourcesToSharedFactory(t *testing.T) {
	w, shared, _ := newRoutingTestWatcher()

	cases := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
		// A secrets resource in a non-core group must NOT be treated as the
		// core Secret kind - it would not expose the `type` field selector.
		{Group: "example.com", Version: "v1", Resource: "secrets"},
	}
	for _, gvr := range cases {
		assert.Same(t, shared, w.factoryForGVR(gvr),
			"non core/v1-Secret GVR %v must route to the shared factory", gvr)
	}
}

func TestSecretWatchFieldSelectorExcludesHelmReleases(t *testing.T) {
	assert.Equal(t, "type!=helm.sh/release.v1", secretWatchFieldSelector)
}

type updateEvent struct{ old, new *unstructured.Unstructured }

func recvOrFail[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestWatch_EndToEndDeliversDecodedObjectsFromCompactStore(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "WidgetList"})
	w := &watcher{
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		activeHandlers:      make(map[utils.ResourceDescriptor]resourceContext),
		informersConfigured: make(map[schema.GroupVersionResource]bool),
		handlers:            make(map[utils.ResourceDescriptor][]*weightedHandler),
		syncedCallbacks:     make(map[utils.ResourceDescriptor][]func()),
		objectSubsAdd:       make(map[objectSubscriptionKey][]func(*unstructured.Unstructured)),
		objectSubsUpdate:    make(map[objectSubscriptionKey][]func(*unstructured.Unstructured)),
		objectSubsDelete:    make(map[objectSubscriptionKey][]func(*unstructured.Unstructured)),
		factoryStopCh:       make(chan struct{}),
		factory:             dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, time.Minute, metav1.NamespaceAll, nil),
		secretFactory:       dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, time.Minute, metav1.NamespaceAll, nil),
	}
	t.Cleanup(func() { close(w.factoryStopCh) })

	res := utils.ResourceDescriptor{Kind: "Widget", Plural: "widgets", ApiVersion: "example.com/v1", Namespaced: true}
	adds := make(chan *unstructured.Unstructured, 8)
	updates := make(chan updateEvent, 8)
	deletes := make(chan *unstructured.Unstructured, 8)

	unwatchHandler, err := w.Watch(res, 0,
		func(_ utils.ResourceDescriptor, u *unstructured.Unstructured) { adds <- u },
		func(_ utils.ResourceDescriptor, o, n *unstructured.Unstructured) { updates <- updateEvent{o, n} },
		func(_ utils.ResourceDescriptor, u *unstructured.Unstructured) { deletes <- u },
	)
	require.NoError(t, err)
	defer unwatchHandler()

	synced := make(chan struct{})
	w.OnSynced(res, func() { close(synced) })
	recvOrFail(t, synced, "informer sync")

	widget := func(rv string, size int64) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "w1", "namespace": "ns1", "resourceVersion": rv},
			"spec":       map[string]any{"size": size, "nested": map[string]any{"list": []any{"a", "b"}}},
		}}
	}
	rc := client.Resource(gvr).Namespace("ns1")
	ctx := context.Background()

	_, err = rc.Create(ctx, widget("1", 1), metav1.CreateOptions{})
	require.NoError(t, err)
	added := recvOrFail(t, adds, "add")
	assert.Equal(t, "w1", added.GetName())
	size, _, err := unstructured.NestedInt64(added.Object, "spec", "size")
	require.NoError(t, err)
	assert.Equal(t, int64(1), size)
	list, _, _ := unstructured.NestedStringSlice(added.Object, "spec", "nested", "list")
	assert.Equal(t, []string{"a", "b"}, list)
	assert.Equal(t, "Widget", added.GetKind())

	// The store holds the compact form.
	items := w.activeHandlers[res].informer.GetStore().List()
	require.Len(t, items, 1)
	assert.IsType(t, &compactObject{}, items[0])

	// Update: old and new both fully decoded.
	cur, err := rc.Get(ctx, "w1", metav1.GetOptions{})
	require.NoError(t, err)
	oldRV := cur.GetResourceVersion()
	require.NoError(t, unstructured.SetNestedField(cur.Object, int64(5), "spec", "size"))
	cur.SetResourceVersion("2")
	_, err = rc.Update(ctx, cur, metav1.UpdateOptions{})
	require.NoError(t, err)
	up := recvOrFail(t, updates, "update")
	assert.Equal(t, oldRV, up.old.GetResourceVersion())
	assert.Equal(t, "2", up.new.GetResourceVersion())
	oldSize, _, _ := unstructured.NestedInt64(up.old.Object, "spec", "size")
	newSize, _, _ := unstructured.NestedInt64(up.new.Object, "spec", "size")
	assert.Equal(t, int64(1), oldSize)
	assert.Equal(t, int64(5), newSize)

	// A second Watch replays decoded objects into its onAdd.
	replayed := make(chan *unstructured.Unstructured, 8)
	unwatch2, err := w.Watch(res, 1,
		func(_ utils.ResourceDescriptor, u *unstructured.Unstructured) { replayed <- u }, nil, nil)
	require.NoError(t, err)
	defer unwatch2()
	r := recvOrFail(t, replayed, "replay")
	assert.Equal(t, "w1", r.GetName())
	replaySize, _, _ := unstructured.NestedInt64(r.Object, "spec", "size")
	assert.Equal(t, int64(5), replaySize)

	// Delete.
	require.NoError(t, rc.Delete(ctx, "w1", metav1.DeleteOptions{}))
	del := recvOrFail(t, deletes, "delete")
	assert.Equal(t, "w1", del.GetName())
	delSize, _, _ := unstructured.NestedInt64(del.Object, "spec", "size")
	assert.Equal(t, int64(5), delSize)

	require.NoError(t, w.Unwatch(res))
}

// Resync passes the identical cached pointer as old and new; the handlers
// must still get decoded objects, sharing one decode.
func TestObjectToUnstructured_ResyncSamePointerIsDetected(t *testing.T) {
	p := testPod("p", "ns", "1")
	p.SetAPIVersion("v1")
	p.SetKind("Pod")
	c, err := newCompactObject(p)
	require.NoError(t, err)
	assert.True(t, sameObject(c, c))
	c2, _ := newCompactObject(p)
	assert.False(t, sameObject(c, c2))
}
