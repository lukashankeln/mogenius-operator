// Package sandboxactivity remembers when a pod last got a toolbox call: exec, a session request, a file
// operation, a port-forward or an SSH connection. The auto-stop of agent sandboxes (services/sandboxautostop.go)
// reads it; a sandbox's pod has the sandbox's name. Calls only mark time, they never block or fail.
package sandboxactivity

import (
	"sync"
	"time"
)

// Tracker keeps the last call per pod and the connections still open. In memory: after a restart the clock of a
// pod starts with what the auto-stop finds on the Sandbox.
type Tracker struct {
	mu   sync.Mutex
	last map[string]time.Time
	open map[string]int
	now  func() time.Time
}

// NewTracker returns an empty tracker with the given clock.
func NewTracker(now func() time.Time) *Tracker {
	return &Tracker{last: map[string]time.Time{}, open: map[string]int{}, now: now}
}

var tracker = NewTracker(time.Now)

// Default is the tracker the operator's handlers write to.
func Default() *Tracker { return tracker }

func key(namespace, pod string) string { return namespace + "/" + pod }

// Touch marks a call for the pod.
func Touch(namespace, pod string) { tracker.Touch(namespace, pod) }

// Hold marks a connection to the pod that lasts until the returned func runs (a stream, a tunnel, an SSH
// session): the pod counts as active all that time.
func Hold(namespace, pod string) func() { return tracker.Hold(namespace, pod) }

func (t *Tracker) Touch(namespace, pod string) {
	if namespace == "" || pod == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[key(namespace, pod)] = t.now()
}

func (t *Tracker) Hold(namespace, pod string) func() {
	if namespace == "" || pod == "" {
		return func() {}
	}
	k := key(namespace, pod)
	t.mu.Lock()
	t.open[k]++
	t.last[k] = t.now()
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.open[k] <= 1 {
				delete(t.open, k)
			} else {
				t.open[k]--
			}
			t.last[k] = t.now()
		})
	}
}

// Last returns the pod's last call and whether a connection to it is open now; the zero time when none was seen.
func (t *Tracker) Last(namespace, pod string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := key(namespace, pod)
	return t.last[k], t.open[k] > 0
}

// Prune forgets the pods keep does not want and that have no open connection, so calls to pods that are no
// sandbox do not pile up.
func (t *Tracker) Prune(keep func(namespace, pod string) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.last {
		if t.open[k] > 0 {
			continue
		}
		namespace, pod := split(k)
		if !keep(namespace, pod) {
			delete(t.last, k)
		}
	}
}

func split(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
