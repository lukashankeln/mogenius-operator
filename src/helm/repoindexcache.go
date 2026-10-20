package helm

import (
	"fmt"
	"os"
	"sync"
	"time"

	"helm.sh/helm/v4/pkg/repo/v1"
)

// loadIndexFile parses a repository index file. It is a variable so tests can
// count how often an index is actually parsed.
var loadIndexFile = repo.LoadIndexFile

// repoIndexSummary is the small part of a repository index that chart search
// and chart version listing need.
//
// Parsing a repository index (bitnami-class indexes are tens of MB of YAML)
// allocates a very large transient heap. Doing that on every request drives
// peak RAM, so the index is parsed once per file change and only this summary
// is retained: one entry per chart in latest, and at most MAXCHART_VERSIONS+1
// entries per chart name in versions.
type repoIndexSummary struct {
	repoName string
	modTime  time.Time
	size     int64

	// latest holds the first version of every chart in the index.
	latest []HelmChartInfo
	// versions holds the versions per chart name in index order, capped at
	// MAXCHART_VERSIONS+1 entries (the historical HelmChartVersion limit).
	versions map[string][]HelmChartInfo
}

var (
	repoIndexCacheMu sync.Mutex
	repoIndexCache   = map[string]*repoIndexSummary{}
)

// loadRepoIndexSummary returns the summary of the index file at path. The
// cached summary is used while the file's modification time and size are
// unchanged; otherwise the index is parsed again. Errors are not cached.
// The returned summary is shared and must be treated as read-only.
func loadRepoIndexSummary(repoName, path string) (*repoIndexSummary, error) {
	// Stat before parsing: if the file is replaced during the parse, the stored
	// stat is the older one and the next call reloads.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	repoIndexCacheMu.Lock()
	cached := repoIndexCache[path]
	repoIndexCacheMu.Unlock()
	if cached != nil && cached.repoName == repoName && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached, nil
	}

	// The parse runs outside the lock; concurrent callers may parse twice,
	// which is preferable to holding the lock during a multi-MB parse.
	indexFile, err := loadIndexFile(path)
	if err != nil {
		return nil, err
	}

	summary := &repoIndexSummary{
		repoName: repoName,
		modTime:  info.ModTime(),
		size:     info.Size(),
		versions: map[string][]HelmChartInfo{},
	}
	for _, chartVersions := range indexFile.Entries {
		for i, chartVersion := range chartVersions {
			chartInfo := HelmChartInfo{
				Name:        fmt.Sprintf("%s/%s", repoName, chartVersion.Name),
				Version:     chartVersion.Version,
				AppVersion:  chartVersion.AppVersion,
				Description: chartVersion.Description,
			}
			if i == 0 {
				summary.latest = append(summary.latest, chartInfo)
			}
			if len(summary.versions[chartVersion.Name]) <= MAXCHART_VERSIONS {
				summary.versions[chartVersion.Name] = append(summary.versions[chartVersion.Name], chartInfo)
			}
		}
	}

	repoIndexCacheMu.Lock()
	repoIndexCache[path] = summary
	repoIndexCacheMu.Unlock()
	return summary, nil
}

// invalidateRepoIndexSummary drops the cached summaries of a repository. It
// must be called whenever the repository's index file is (re)written or the
// repository is removed.
func invalidateRepoIndexSummary(repoName string) {
	repoIndexCacheMu.Lock()
	defer repoIndexCacheMu.Unlock()
	for path, summary := range repoIndexCache {
		if summary.repoName == repoName {
			delete(repoIndexCache, path)
		}
	}
}

// pruneRepoIndexSummaries drops cached summaries whose path is not in keep, so
// removed repositories do not retain memory.
func pruneRepoIndexSummaries(keep map[string]struct{}) {
	repoIndexCacheMu.Lock()
	defer repoIndexCacheMu.Unlock()
	for path := range repoIndexCache {
		if _, ok := keep[path]; !ok {
			delete(repoIndexCache, path)
		}
	}
}
