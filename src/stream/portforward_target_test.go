package stream

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testPod(name string, phase corev1.PodPhase, ready bool, terminating bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
	}
	return pod
}

// A pod being deleted still reports phase Running, so a rollout would hand
// the tunnel the pod that is about to vanish unless it is skipped.
func TestPickRunningPod(t *testing.T) {
	tests := []struct {
		name string
		pods []corev1.Pod
		want string
		ok   bool
	}{
		{
			name: "skips a terminating pod even though it is ready",
			pods: []corev1.Pod{testPod("old", corev1.PodRunning, true, true), testPod("new", corev1.PodRunning, true, false)},
			want: "new", ok: true,
		},
		{
			name: "prefers a ready pod over one still starting",
			pods: []corev1.Pod{testPod("starting", corev1.PodRunning, false, false), testPod("ready", corev1.PodRunning, true, false)},
			want: "ready", ok: true,
		},
		{
			name: "falls back to a running pod that is not ready yet",
			pods: []corev1.Pod{testPod("old", corev1.PodRunning, true, true), testPod("starting", corev1.PodRunning, false, false)},
			want: "starting", ok: true,
		},
		{
			name: "finds nothing when only terminating or pending pods exist",
			pods: []corev1.Pod{testPod("old", corev1.PodRunning, true, true), testPod("pending", corev1.PodPending, false, false)},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickRunningPod(tt.pods)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolvePortForwardTargetSkipsTerminatingPod(t *testing.T) {
	old := testPod("web-old", corev1.PodRunning, true, true)
	next := testPod("web-new", corev1.PodRunning, true, false)
	clientset := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
		},
		&old, &next,
	)

	pod, err := resolvePortForwardTarget(clientset, PortForwardConnectionRequest{Kind: "deployment", Namespace: "ns", WorkloadName: "web"})

	require.NoError(t, err)
	assert.Equal(t, "web-new", pod)
}

func TestPfDialErrorReasonNamesMissingPod(t *testing.T) {
	assert.Equal(t, "no running pod for the tunnel target", pfDialErrorReason(errNoRunningPod))
}

// fakeForwards hands out forwards whose end the test controls, and whose
// dial address is a real local listener (or a closed one, for a dead forward).
type fakeForwards struct {
	mu       sync.Mutex
	forwards map[string]*podForward
	lost     map[string]chan struct{}
	started  []string
}

func newFakeForwards() *fakeForwards {
	return &fakeForwards{forwards: map[string]*podForward{}, lost: map[string]chan struct{}{}}
}

func (f *fakeForwards) start(t *testing.T, alive bool) func(string) (*podForward, error) {
	return func(pod string) (*podForward, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		if alive {
			t.Cleanup(func() { _ = listener.Close() })
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
		} else {
			_ = listener.Close()
		}
		lost := make(chan struct{})
		forward := &podForward{podName: pod, dialAddr: listener.Addr().String(), stop: make(chan struct{}), lost: lost}
		f.mu.Lock()
		f.forwards[pod], f.lost[pod] = forward, lost
		f.started = append(f.started, pod)
		f.mu.Unlock()
		return forward, nil
	}
}

func (f *fakeForwards) lose(pod string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.lost[pod])
}

func (f *fakeForwards) startedPods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// holdRecorder records which pods are held, to check the activity hold moves.
type holdRecorder struct {
	mu   sync.Mutex
	open map[string]int
}

func (h *holdRecorder) hold(namespace, pod string) func() {
	h.mu.Lock()
	h.open[pod]++
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		h.open[pod]--
		h.mu.Unlock()
	}
}

func (h *holdRecorder) count(pod string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open[pod]
}

func testForwarder(resolve func() (string, error), start func(string) (*podForward, error), holds *holdRecorder) *podForwarder {
	hold := &podHold{namespace: "ns", hold: holds.hold}
	f := newPodForwarder(slog.New(slog.NewTextHandler(io.Discard, nil)), resolve, start, hold)
	f.minBackoff, f.maxBackoff = time.Millisecond, 4*time.Millisecond
	f.rebuildDialWait = 200 * time.Millisecond
	return f
}

// currentPod reads the forwarder's pod under its lock; run swaps it concurrently.
func currentPod(f *podForwarder) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return podNameOf(f.current)
}

func sequence(pods ...string) func() (string, error) {
	var mu sync.Mutex
	i := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		pod := pods[min(i, len(pods)-1)]
		i++
		if pod == "" {
			return "", errors.New("no running pods")
		}
		return pod, nil
	}
}

// The bug: the tunnel bound itself to the pod resolved at open time and never
// noticed it was gone. Now losing the forward moves the tunnel to the new pod.
func TestPodForwarderRebuildsOnTheNewPodWhenThePodGoesAway(t *testing.T) {
	forwards := newFakeForwards()
	holds := &holdRecorder{open: map[string]int{}}
	f := testForwarder(sequence("web-new"), forwards.start(t, true), holds)
	done := make(chan struct{})
	defer close(done)

	require.NoError(t, f.startOn("web-old"))
	go f.run(done)
	assert.Equal(t, 1, holds.count("web-old"))

	forwards.lose("web-old")

	assert.Eventually(t, func() bool { return currentPod(f) == "web-new" }, time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{"web-old", "web-new"}, forwards.startedPods())
	assert.Equal(t, 0, holds.count("web-old"))
	assert.Equal(t, 1, holds.count("web-new"))
}

func TestPodForwarderKeepsRetryingWhileNoPodRuns(t *testing.T) {
	forwards := newFakeForwards()
	holds := &holdRecorder{open: map[string]int{}}
	// Scaled to zero for two attempts, then back up.
	f := testForwarder(sequence("", "", "web-back"), forwards.start(t, true), holds)
	done := make(chan struct{})
	defer close(done)

	require.NoError(t, f.startOn("web-old"))
	go f.run(done)
	forwards.lose("web-old")

	assert.Eventually(t, func() bool { return currentPod(f) == "web-back" }, time.Second, 5*time.Millisecond)
}

// A dial can fail before the lost forward is noticed; it must not wait for
// that, but trigger the rebuild itself and use the new forward.
func TestPodForwarderDialRebuildsAfterAFailedDial(t *testing.T) {
	forwards := newFakeForwards()
	holds := &holdRecorder{open: map[string]int{}}
	start := forwards.start(t, true)
	dead := forwards.start(t, false)
	f := testForwarder(sequence("web-new"), start, holds)
	done := make(chan struct{})
	defer close(done)

	// The first forward's local end is already gone, but its loss was not reported.
	initial, err := dead("web-old")
	require.NoError(t, err)
	f.set(initial)
	go f.run(done)

	conn, err := f.dial(done, time.Second)

	require.NoError(t, err)
	_ = conn.Close()
	assert.Equal(t, "web-new", currentPod(f))
}

func TestPodForwarderDialReportsNoRunningPod(t *testing.T) {
	forwards := newFakeForwards()
	holds := &holdRecorder{open: map[string]int{}}
	dead := forwards.start(t, false)
	f := testForwarder(sequence(""), forwards.start(t, true), holds)
	done := make(chan struct{})
	defer close(done)

	initial, err := dead("web-old")
	require.NoError(t, err)
	f.set(initial)
	go f.run(done)

	_, err = f.dial(done, time.Second)

	assert.ErrorIs(t, err, errNoRunningPod)
}

func TestPodForwarderCloseStopsTheForwardAndReleasesTheHold(t *testing.T) {
	forwards := newFakeForwards()
	holds := &holdRecorder{open: map[string]int{}}
	f := testForwarder(sequence("web"), forwards.start(t, true), holds)

	require.NoError(t, f.startOn("web"))
	f.mu.Lock()
	current := f.current
	f.mu.Unlock()
	f.close()

	select {
	case <-current.stop:
	default:
		t.Fatal("the forward was not stopped")
	}
	assert.Equal(t, 0, holds.count("web"))
}
