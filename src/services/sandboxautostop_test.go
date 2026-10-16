package services

import (
	"testing"
	"time"

	"mogenius-operator/src/sandboxactivity"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func sandboxObject(annotations map[string]any, mode string, ready string, readySince string, owner string) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1",
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": "sb", "namespace": "ns", "annotations": annotations},
		"spec":       map[string]any{"operatingMode": mode},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": ready, "lastTransitionTime": readySince},
		}},
	}
	if owner != "" {
		obj["metadata"].(map[string]any)["ownerReferences"] = []any{
			map[string]any{"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim", "name": owner, "uid": "u"},
		}
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestSandboxIdleStateOf(t *testing.T) {
	claims := map[string]time.Duration{"ns/claim": 30 * time.Minute}

	fromClaim := sandboxIdleStateOf(sandboxObject(map[string]any{}, "Running", "True", "2026-10-16T10:00:00Z", "claim"), claims)
	if fromClaim.Interval != 30*time.Minute || !fromClaim.Running || !fromClaim.Ready ||
		!fromClaim.ReadySince.Equal(time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("from claim = %+v", fromClaim)
	}

	own := sandboxIdleStateOf(sandboxObject(map[string]any{
		sandboxAutoStopAnnotation:     "0",
		sandboxLastActivityAnnotation: "2026-10-16T11:00:00Z",
	}, "Suspended", "False", "2026-10-16T10:00:00Z", "claim"), claims)
	if own.Interval != 0 || own.Running || own.Ready || !own.ReadySince.IsZero() ||
		!own.Stored.Equal(time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("own annotation wins = %+v", own)
	}

	for raw, want := range map[string]time.Duration{"15": 15 * time.Minute, "0": 0, "-3": 0, "x": 0, "": 0} {
		if got := intervalOf(raw); got != want {
			t.Errorf("intervalOf(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestDecideSandbox(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-24 * time.Hour)
	idle := sandboxIdleState{Namespace: "ns", Name: "sb", Interval: 30 * time.Minute, Running: true, Ready: true,
		ReadySince: now.Add(-2 * time.Hour)}
	empty := func() *sandboxactivity.Tracker { return sandboxactivity.NewTracker(func() time.Time { return now }) }

	if d := decideSandbox(idle, empty(), false, now, startedAt); !d.Stop || d.WriteActivity {
		t.Fatalf("idle two hours, no calls = %+v", d)
	}

	recent := sandboxactivity.NewTracker(func() time.Time { return now.Add(-10 * time.Minute) })
	recent.Touch("ns", "sb")
	if d := decideSandbox(idle, recent, false, now, startedAt); d.Stop || !d.WriteActivity {
		t.Fatalf("a call ten minutes ago = %+v", d)
	}

	stored := idle
	stored.Stored = now.Add(-5 * time.Minute)
	if d := decideSandbox(stored, empty(), false, now, startedAt); d.Stop {
		t.Fatalf("the stored activity counts after a restart = %+v", d)
	}
	if d := decideSandbox(idle, empty(), false, now, now.Add(-5*time.Minute)); d.Stop {
		t.Fatalf("an operator that just started stops nothing = %+v", d)
	}

	streaming := empty()
	release := streaming.Hold("ns", "sb")
	defer release()
	if d := decideSandbox(idle, streaming, false, now.Add(time.Hour), startedAt); d.Stop {
		t.Fatalf("an open connection keeps it = %+v", d)
	}
	if d := decideSandbox(idle, empty(), true, now, startedAt); d.Stop {
		t.Fatalf("a running session command keeps it = %+v", d)
	}

	off := idle
	off.Interval = 0
	stopped := idle
	stopped.Running = false
	starting := idle
	starting.Ready = false
	for name, s := range map[string]sandboxIdleState{"no interval": off, "already stopped": stopped, "not ready": starting} {
		if d := decideSandbox(s, empty(), false, now, startedAt); d.Stop {
			t.Errorf("%s = %+v", name, d)
		}
	}
}

func TestPodSessionsBusy(t *testing.T) {
	sessions.mu.Lock()
	saved := sessions.sessions
	sessions.sessions = map[string]*podSession{
		"a": {namespace: "ns", pod: "sb", current: &sessionCommand{}},
		"b": {namespace: "ns", pod: "quiet"},
	}
	sessions.mu.Unlock()
	defer func() {
		sessions.mu.Lock()
		sessions.sessions = saved
		sessions.mu.Unlock()
	}()
	if !podSessionsBusy("ns", "sb") || podSessionsBusy("ns", "quiet") || podSessionsBusy("ns", "none") {
		t.Fatal("busy = a running command, nothing else")
	}
}
