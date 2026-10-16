package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"mogenius-operator/src/sandboxactivity"
	"mogenius-operator/src/shutdown"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Agent sandboxes with an auto-stop interval (MOG-4697) are stopped after that many minutes without a toolbox
// call: `spec.operatingMode: Suspended`, the pod goes, the volume stays, and a start brings the same sandbox
// back. The interval is an annotation the platform API writes on the claim or the sandbox (the sandbox's wins).
// Activity comes from sandboxactivity; at most once a minute it is written to the sandbox as an annotation, so
// a restart of the operator stops nothing early. Only the leader stops sandboxes.

const (
	sandboxAutoStopAnnotation     = "mogenius.io/sandbox-auto-stop-interval"
	sandboxLastActivityAnnotation = "mogenius.io/sandbox-last-activity"
	sandboxAutoStopTick           = time.Minute
	// sandboxActivityWriteEvery is how much newer the activity must be than the stored one to be written.
	sandboxActivityWriteEvery = time.Minute
)

var (
	sandboxGVR      = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	sandboxClaimGVR = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
)

// sandboxIdleState is what the auto-stop decides on for one sandbox.
type sandboxIdleState struct {
	Namespace string
	Name      string
	Interval  time.Duration
	Running   bool
	Ready     bool
	// ReadySince is when the sandbox last became ready (a start counts as activity).
	ReadySince time.Time
	// Stored is the activity annotation on the sandbox.
	Stored time.Time
}

// sandboxDecision is the outcome for one sandbox: whether to write newer activity and whether to stop it.
type sandboxDecision struct {
	LastActive    time.Time
	WriteActivity bool
	Stop          bool
}

// decideSandbox weighs the sandbox against the tracker. startedAt is when the operator started: a sandbox it has
// not seen a call for since then is idle at most since then.
func decideSandbox(s sandboxIdleState, tracker *sandboxactivity.Tracker, busy bool, now, startedAt time.Time) sandboxDecision {
	seen, open := tracker.Last(s.Namespace, s.Name)
	last := latest(seen, s.Stored, s.ReadySince, startedAt)
	d := sandboxDecision{LastActive: last}
	d.WriteActivity = s.Interval > 0 && !seen.IsZero() && seen.Sub(s.Stored) >= sandboxActivityWriteEvery
	d.Stop = s.Interval > 0 && s.Running && s.Ready && !open && !busy && now.Sub(last) > s.Interval
	return d
}

func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// sandboxIdleStateOf reads a Sandbox object; claimIntervals are the intervals of the claims by namespace/name.
func sandboxIdleStateOf(obj *unstructured.Unstructured, claimIntervals map[string]time.Duration) sandboxIdleState {
	s := sandboxIdleState{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	annotations := obj.GetAnnotations()
	if raw, ok := annotations[sandboxAutoStopAnnotation]; ok {
		s.Interval = intervalOf(raw)
	} else {
		for _, owner := range obj.GetOwnerReferences() {
			if owner.Kind == "SandboxClaim" {
				s.Interval = claimIntervals[obj.GetNamespace()+"/"+owner.Name]
			}
		}
	}
	if at, err := time.Parse(time.RFC3339, annotations[sandboxLastActivityAnnotation]); err == nil {
		s.Stored = at
	}
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	s.Running = mode == "" || mode == "Running"
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		s.Ready = cond["status"] == "True"
		if raw, ok := cond["lastTransitionTime"].(string); ok && s.Ready {
			s.ReadySince, _ = time.Parse(time.RFC3339, raw)
		}
	}
	return s
}

// intervalOf reads the annotation's minutes; anything else than a positive number is no auto-stop.
func intervalOf(raw string) time.Duration {
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}

// StartSandboxAutoStop runs the auto-stop once a minute while isLeading says this replica leads. Started once
// from cmd.startClusterSystems.
func StartSandboxAutoStop(isLeading func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdown.Add(cancel)
	startedAt := time.Now()

	go func() {
		ticker := time.NewTicker(sandboxAutoStopTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if isLeading() {
					stopIdleSandboxes(ctx, startedAt)
				}
			}
		}
	}()
}

func stopIdleSandboxes(ctx context.Context, startedAt time.Time) {
	logger := serviceLogger.With("scope", "sandbox-auto-stop")
	client := clientProvider.DynamicClient()
	list, err := client.Resource(sandboxGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		// No agent-sandbox on this cluster is the common case; nothing to do.
		logger.Debug("sandbox auto-stop: list sandboxes", "error", err)
		return
	}
	claimIntervals := map[string]time.Duration{}
	if claims, err := client.Resource(sandboxClaimGVR).Namespace("").List(ctx, metav1.ListOptions{}); err == nil {
		for _, c := range claims.Items {
			if interval := intervalOf(c.GetAnnotations()[sandboxAutoStopAnnotation]); interval > 0 {
				claimIntervals[c.GetNamespace()+"/"+c.GetName()] = interval
			}
		}
	}

	tracker := sandboxactivity.Default()
	now := time.Now()
	sandboxes := map[string]bool{}
	for i := range list.Items {
		s := sandboxIdleStateOf(&list.Items[i], claimIntervals)
		sandboxes[s.Namespace+"/"+s.Name] = true
		d := decideSandbox(s, tracker, podSessionsBusy(s.Namespace, s.Name), now, startedAt)
		if d.WriteActivity {
			seen, _ := tracker.Last(s.Namespace, s.Name)
			patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
				sandboxLastActivityAnnotation: seen.UTC().Format(time.RFC3339),
			}}}
			if err := mergePatchSandbox(ctx, s.Namespace, s.Name, patch); err != nil {
				logger.Warn("sandbox auto-stop: write activity", "namespace", s.Namespace, "sandbox", s.Name, "error", err)
			}
		}
		if d.Stop {
			patch := map[string]any{"spec": map[string]any{"operatingMode": "Suspended"}}
			if err := mergePatchSandbox(ctx, s.Namespace, s.Name, patch); err != nil {
				logger.Warn("sandbox auto-stop: stop", "namespace", s.Namespace, "sandbox", s.Name, "error", err)
				continue
			}
			logger.Info("sandbox auto-stop: stopped idle sandbox", "namespace", s.Namespace, "sandbox", s.Name,
				"idle", now.Sub(d.LastActive).Round(time.Second).String(), "interval", s.Interval.String())
		}
	}
	tracker.Prune(func(namespace, pod string) bool { return sandboxes[namespace+"/"+pod] })
}

// mergePatchSandbox applies a JSON merge patch to a Sandbox: only the given keys change, so it does not race the
// platform's whole-object writes of other fields.
func mergePatchSandbox(ctx context.Context, namespace, name string, patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch for sandbox %s/%s: %w", namespace, name, err)
	}
	if _, err := clientProvider.DynamicClient().Resource(sandboxGVR).Namespace(namespace).
		Patch(ctx, name, types.MergePatchType, raw, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch sandbox %s/%s: %w", namespace, name, err)
	}
	return nil
}
