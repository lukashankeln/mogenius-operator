package gitops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func sandboxArtifact() GitOpsArtifact {
	return GitOpsArtifact{
		Namespace: "agent-sandbox-system",
		HelmChart: HelmChartReference{
			Repository: "https://helm.mogenius.com/public",
			Chart:      "mogenius-agent-sandbox",
			Name:       "agent-sandbox",
			Version:    "1.3.0",
		},
	}
}

// Without an explicit opt-in the Application carries no ignoreDifferences and the
// sync options stay as they were, so every other component is unaffected.
func TestBuildArgoApplicationNoIgnoreDifferencesByDefault(t *testing.T) {
	app := buildArgoApplication("agent-sandboxes", sandboxArtifact(), "argocd")

	_, found, err := unstructured.NestedSlice(app.Object, "spec", "ignoreDifferences")
	require.NoError(t, err)
	assert.False(t, found, "no ignoreDifferences unless the component asks for it")

	opts, _, err := unstructured.NestedSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	require.NoError(t, err)
	assert.Equal(t, []any{"CreateNamespace=true", "ServerSideApply=true"}, opts)
	assert.NotContains(t, opts, "RespectIgnoreDifferences=true")
}

// MOG-4827: a component can ignore a field its controller owns so the Application
// stops sitting Healthy/OutOfSync; the ignore only ends the selfHeal loop when the
// sync respects it, so RespectIgnoreDifferences has to come along.
func TestBuildArgoApplicationWritesIgnoreDifferences(t *testing.T) {
	artifact := sandboxArtifact()
	ignore := []any{
		map[string]any{
			"group": "extensions.agents.x-k8s.io",
			"kind":  "SandboxTemplate",
			"jsonPointers": []any{
				"/metadata/labels/agents.x-k8s.io~1sandbox-template-ref-hash",
			},
		},
	}
	artifact.ArgoCD = &ArgoCDSettings{Project: "mogenius", IgnoreDifferences: ignore}

	app := buildArgoApplication("agent-sandboxes", artifact, "argocd")

	got, found, err := unstructured.NestedSlice(app.Object, "spec", "ignoreDifferences")
	require.NoError(t, err)
	require.True(t, found, "ignoreDifferences must be written when the component sets it")
	assert.Equal(t, ignore, got)

	opts, _, err := unstructured.NestedSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	require.NoError(t, err)
	assert.Contains(t, opts, "RespectIgnoreDifferences=true")
}
