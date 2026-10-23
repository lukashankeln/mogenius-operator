package helm

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/repo/v1"
)

type testChart struct {
	name     string
	versions int
}

// writeTestIndex writes an index file with the given charts. Version N of a
// chart is "1.<N>.0"; after SortEntries the newest (highest N) comes first.
func writeTestIndex(t *testing.T, path string, mtime time.Time, charts ...testChart) {
	t.Helper()
	idx := repo.NewIndexFile()
	for _, c := range charts {
		for n := 1; n <= c.versions; n++ {
			md := &chart.Metadata{
				APIVersion:  "v2",
				Name:        c.name,
				Version:     fmt.Sprintf("1.%d.0", n),
				AppVersion:  fmt.Sprintf("app-%d", n),
				Description: "desc of " + c.name,
			}
			require.NoError(t, idx.MustAdd(md, fmt.Sprintf("%s-%d.tgz", c.name, n), "http://example.invalid", "sha256:abc"))
		}
	}
	idx.SortEntries()
	require.NoError(t, idx.WriteFile(path, 0644))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func countLoads(t *testing.T) *int {
	t.Helper()
	calls := 0
	orig := loadIndexFile
	loadIndexFile = func(p string) (*repo.IndexFile, error) {
		calls++
		return orig(p)
	}
	t.Cleanup(func() { loadIndexFile = orig })
	return &calls
}

func resetRepoIndexCache(t *testing.T) {
	t.Helper()
	repoIndexCacheMu.Lock()
	repoIndexCache = map[string]*repoIndexSummary{}
	repoIndexCacheMu.Unlock()
	t.Cleanup(func() {
		repoIndexCacheMu.Lock()
		repoIndexCache = map[string]*repoIndexSummary{}
		repoIndexCacheMu.Unlock()
	})
}

var baseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestLoadRepoIndexSummary_Derivation(t *testing.T) {
	resetRepoIndexCache(t)
	path := filepath.Join(t.TempDir(), "r-index.yaml")
	writeTestIndex(t, path, baseTime, testChart{"big", 60}, testChart{"small", 3})

	s, err := loadRepoIndexSummary("r", path)
	require.NoError(t, err)

	// latest: newest version of every chart
	latest := map[string]HelmChartInfo{}
	for _, c := range s.latest {
		latest[c.Name] = c
	}
	assert.Len(t, s.latest, 2)
	assert.Equal(t, HelmChartInfo{Name: "r/big", Version: "1.60.0", AppVersion: "app-60", Description: "desc of big"}, latest["r/big"])
	assert.Equal(t, "1.3.0", latest["r/small"].Version)

	// versions: index order (newest first), capped at MAXCHART_VERSIONS+1
	big := s.versions["big"]
	require.Len(t, big, MAXCHART_VERSIONS+1)
	assert.Equal(t, "1.60.0", big[0].Version)
	assert.Equal(t, "1.10.0", big[MAXCHART_VERSIONS].Version)
	assert.Equal(t, "r/big", big[0].Name)

	small := s.versions["small"]
	require.Len(t, small, 3)
	assert.Equal(t, "1.3.0", small[0].Version)
	assert.Equal(t, "1.1.0", small[2].Version)
}

func TestLoadRepoIndexSummary_CachedUntilFileChanges(t *testing.T) {
	resetRepoIndexCache(t)
	calls := countLoads(t)
	path := filepath.Join(t.TempDir(), "r-index.yaml")
	writeTestIndex(t, path, baseTime, testChart{"a", 1})

	for range 3 {
		_, err := loadRepoIndexSummary("r", path)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, *calls)

	// different size and mtime
	writeTestIndex(t, path, baseTime.Add(time.Hour), testChart{"a", 2}, testChart{"b", 1})
	s, err := loadRepoIndexSummary("r", path)
	require.NoError(t, err)
	assert.Equal(t, 2, *calls)
	assert.Len(t, s.latest, 2)

	// same size, only mtime differs
	require.NoError(t, os.Chtimes(path, baseTime.Add(2*time.Hour), baseTime.Add(2*time.Hour)))
	_, err = loadRepoIndexSummary("r", path)
	require.NoError(t, err)
	assert.Equal(t, 3, *calls)
}

func TestInvalidateRepoIndexSummary(t *testing.T) {
	resetRepoIndexCache(t)
	calls := countLoads(t)
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a-index.yaml")
	pathB := filepath.Join(dir, "b-index.yaml")
	writeTestIndex(t, pathA, baseTime, testChart{"x", 1})
	writeTestIndex(t, pathB, baseTime, testChart{"y", 1})

	_, err := loadRepoIndexSummary("a", pathA)
	require.NoError(t, err)
	_, err = loadRepoIndexSummary("b", pathB)
	require.NoError(t, err)
	require.Equal(t, 2, *calls)

	invalidateRepoIndexSummary("a")

	_, err = loadRepoIndexSummary("b", pathB)
	require.NoError(t, err)
	assert.Equal(t, 2, *calls, "other repos stay cached")
	_, err = loadRepoIndexSummary("a", pathA)
	require.NoError(t, err)
	assert.Equal(t, 3, *calls, "invalidated repo is reloaded")
}

func TestLoadRepoIndexSummary_ErrorsNotCached(t *testing.T) {
	resetRepoIndexCache(t)
	calls := countLoads(t)
	path := filepath.Join(t.TempDir(), "r-index.yaml")

	_, err := loadRepoIndexSummary("r", path)
	require.Error(t, err, "missing file")

	// unparsable file: stat succeeds, loader fails
	require.NoError(t, os.WriteFile(path, []byte("entries: [not a map"), 0644))
	_, err = loadRepoIndexSummary("r", path)
	require.Error(t, err)
	_, err = loadRepoIndexSummary("r", path)
	require.Error(t, err)
	assert.Equal(t, 2, *calls, "failed parse is retried")

	writeTestIndex(t, path, baseTime, testChart{"a", 1})
	_, err = loadRepoIndexSummary("r", path)
	require.NoError(t, err)
}

// setupHelmTestEnv points the package at a temp repositories file and index
// cache containing the given repos (name -> charts).
func setupHelmTestEnv(t *testing.T, repos map[string][]testChart) string {
	t.Helper()
	resetRepoIndexCache(t)

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0755))

	origCfg, origCache, origLogger := repositoryConfig, repositoryCache, helmLogger
	repositoryConfig = filepath.Join(dir, "repositories.yaml")
	repositoryCache = cacheDir
	helmLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { repositoryConfig, repositoryCache, helmLogger = origCfg, origCache, origLogger })

	rf := repo.NewFile()
	for name, charts := range repos {
		rf.Add(&repo.Entry{Name: name, URL: "http://" + name + ".invalid"})
		writeTestIndex(t, filepath.Join(cacheDir, name+"-index.yaml"), baseTime, charts...)
	}
	require.NoError(t, rf.WriteFile(repositoryConfig, 0644))
	return cacheDir
}

func sortedNames(charts []HelmChartInfo) []string {
	names := make([]string, 0, len(charts))
	for _, c := range charts {
		names = append(names, c.Name+"@"+c.Version)
	}
	sort.Strings(names)
	return names
}

func TestHelmChartSearch_UsesSummaryAndPrunes(t *testing.T) {
	cacheDir := setupHelmTestEnv(t, map[string][]testChart{
		"one": {{"nginx", 3}, {"redis", 2}},
		"two": {{"nginx", 1}},
	})
	calls := countLoads(t)

	got, err := HelmChartSearch(HelmChartSearchRequest{Name: "nginx"})
	require.NoError(t, err)
	assert.Equal(t, []string{"one/nginx@1.3.0", "two/nginx@1.1.0"}, sortedNames(got))

	got, err = HelmChartSearch(HelmChartSearchRequest{Name: ""})
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.Equal(t, 2, *calls, "each index parsed once across searches")

	// a repo missing from repositories.yaml is pruned from the cache
	rf := repo.NewFile()
	rf.Add(&repo.Entry{Name: "one", URL: "http://one.invalid"})
	require.NoError(t, rf.WriteFile(repositoryConfig, 0644))
	_, err = HelmChartSearch(HelmChartSearchRequest{Name: ""})
	require.NoError(t, err)
	repoIndexCacheMu.Lock()
	_, hasTwo := repoIndexCache[filepath.Join(cacheDir, "two-index.yaml")]
	n := len(repoIndexCache)
	repoIndexCacheMu.Unlock()
	assert.False(t, hasTwo)
	assert.Equal(t, 1, n)
}

func TestHelmChartVersion_CapAndCopies(t *testing.T) {
	setupHelmTestEnv(t, map[string][]testChart{
		"one": {{"big", 60}, {"other", 2}},
	})

	got, err := HelmChartVersion(HelmChartVersionRequest{Chart: "one/big"})
	require.NoError(t, err)
	require.Len(t, got, MAXCHART_VERSIONS+1)
	assert.Equal(t, "1.60.0", got[0].Version)

	// mutating the result must not affect later results
	got[0].Version = "mutated"
	got = append(got[:1], got[2:]...)
	_ = got

	again, err := HelmChartVersion(HelmChartVersionRequest{Chart: "one/big"})
	require.NoError(t, err)
	require.Len(t, again, MAXCHART_VERSIONS+1)
	assert.Equal(t, "1.60.0", again[0].Version)
	assert.Equal(t, "1.59.0", again[1].Version)

	other, err := HelmChartVersion(HelmChartVersionRequest{Chart: "one/other"})
	require.NoError(t, err)
	assert.Len(t, other, 2)

	none, err := HelmChartVersion(HelmChartVersionRequest{Chart: "one/missing"})
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = HelmChartVersion(HelmChartVersionRequest{Chart: "invalid"})
	assert.Error(t, err)
}
