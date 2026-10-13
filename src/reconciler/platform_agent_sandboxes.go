package reconciler

import (
	"context"
	"fmt"
	"mogenius-operator/src/crds/v1alpha1"
	"mogenius-operator/src/gitops"
	"net"
)

// reconcileAgentSandboxes installs the mogenius agent-sandbox stack as a
// platform component. It reuses the existing mogenius-agent-sandbox Helm chart
// (published to https://helm.mogenius.com/public) as the install unit — the
// chart ships the sandbox namespace, the mogenius-provided default
// SandboxTemplate/WarmPool objects and the egress NetworkPolicies, so there are
// no extra objects to emit here. The spec only templates the chart's values.
//
// The value keys written below (sandboxes.namespace.name,
// sandboxes.networkPolicy.{managed,additionalBlockedCidrs}) are honoured by the
// chart as-is; leaving a field unset keeps the chart / platform-defaults default.
func (d *reconcilerModule) reconcileAgentSandboxes(ctx context.Context, spec v1alpha1.PlatformConfigSpec, installer gitops.GitOpsInstaller, op operation) *ReconcileResult {
	c := spec.AgentSandboxes
	// Not declared: leave whatever is installed alone (see reconcileComponent).
	if c == nil {
		return nil
	}
	return d.reconcileComponent(ctx, spec, installer, op,
		componentSpec{
			enabled:          c.Enabled,
			chart:            c.Chart,
			patches:          c.Patches,
			name:             componentAgentSandboxes,
			defaultChart:     "mogenius-agent-sandbox",
			defaultRepo:      "https://helm.mogenius.com/public",
			defaultName:      "agent-sandbox",
			defaultNamespace: "agent-sandbox-system",
			// The agent-sandbox controller stamps a content-hash label on the
			// SandboxTemplate after Argo applies it. Argo never sends that label,
			// so the Application sits Healthy/OutOfSync and selfHeals forever
			// (MOG-4827). Ignore the controller-owned label; the rest still syncs.
			argoIgnoreDifferences: []any{
				map[string]any{
					"group": "extensions.agents.x-k8s.io",
					"kind":  "SandboxTemplate",
					"jsonPointers": []any{
						"/metadata/labels/agents.x-k8s.io~1sandbox-template-ref-hash",
					},
				},
			},
		},
		func(ctx context.Context) ([]any, error) {
			return []any{}, nil
		},
		func(ctx context.Context) (map[string]any, error) {
			return buildAgentSandboxValues(c)
		},
	)
}

// buildAgentSandboxValues templates the spec into the chart's `sandboxes` values
// tree. Only keys the spec actually sets are written, so an unset field keeps the
// chart / platform-defaults default instead of being forced to its zero value.
// Returns nil when there is nothing to override.
//
// The CRD schema already rejects malformed CIDRs; the check here covers an
// apiserver that does not enforce the format and fails the component with a
// readable reason instead of handing the engine a NetworkPolicy it cannot apply.
func buildAgentSandboxValues(c *v1alpha1.AgentSandboxesConfig) (map[string]any, error) {
	sandboxes := map[string]any{}

	if c.Namespace != "" {
		sandboxes["namespace"] = map[string]any{"name": c.Namespace}
	}

	if np := c.NetworkPolicy; np != nil {
		networkPolicy := map[string]any{}
		if np.Managed != nil {
			networkPolicy["managed"] = *np.Managed
		}
		for _, cidr := range np.AdditionalBlockedCidrs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return nil, fmt.Errorf("agentSandboxes.networkPolicy.additionalBlockedCidrs: invalid CIDR %q: %w", cidr, err)
			}
		}
		if len(np.AdditionalBlockedCidrs) > 0 {
			networkPolicy["additionalBlockedCidrs"] = np.AdditionalBlockedCidrs
		}
		if len(networkPolicy) > 0 {
			sandboxes["networkPolicy"] = networkPolicy
		}
	}

	if len(sandboxes) == 0 {
		return nil, nil
	}
	return map[string]any{"sandboxes": sandboxes}, nil
}
