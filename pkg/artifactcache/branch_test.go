package artifactcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
)

const testCacheVersion = "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"

func TestNormalizeBranch(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{name: "empty", ref: "", want: DetachedHeadScope},
		{name: "whitespace", ref: "   ", want: DetachedHeadScope},
		{name: "head", ref: "HEAD", want: DetachedHeadScope},
		{name: "plain branch", ref: "main", want: "main"},
		{name: "full ref", ref: "refs/heads/main", want: "main"},
		{name: "branch with slash", ref: "refs/heads/feature/login", want: "feature/login"},
		{name: "plain branch with slash", ref: "feature/login", want: "feature/login"},
		{name: "tag ref", ref: "refs/tags/v1.2.3", want: DetachedHeadScope},
		{name: "pull ref", ref: "refs/pull/42/merge", want: DetachedHeadScope},
		{name: "path traversal", ref: "../../../etc/passwd", want: "etc/passwd"},
		{name: "absolute traversal", ref: "/../../etc/passwd", want: "etc/passwd"},
		{name: "backslash", ref: `feature\..\bar`, want: "feature/bar"},
		{name: "control chars", ref: "feat\x00ure", want: "feat-ure"},
		{name: "git specials", ref: "a~b^c:d?e*f[g", want: "a-b-c-d-e-f-g"},
		{name: "duplicated slashes and dots", ref: "feature//./login/", want: "feature/login"},
		{name: "lock suffix", ref: "weird.lock", want: "weird-lock"},
		{name: "at brace", ref: "foo@{bar", want: "foo--bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeBranch(tc.ref))
		})
	}
}

func TestNormalizeBranch_PathSafety(t *testing.T) {
	// The normalized name must never escape a base directory when used in a
	// path, even though artifacts themselves are stored by numeric id.
	base := t.TempDir()
	dangerous := []string{
		"../../../etc/passwd",
		"..\\..\\windows",
		"/abs/path",
		"feature/../../escape",
		"a/../../b",
	}
	for _, ref := range dangerous {
		scope := NormalizeBranch(ref)
		assert.NotContains(t, scope, "..")
		assert.False(t, strings.HasPrefix(scope, "/"))
		assert.False(t, strings.ContainsRune(scope, '\\'))
		joined := filepath.Join(base, scope)
		rel, err := filepath.Rel(base, joined)
		require.NoError(t, err)
		assert.False(t, strings.HasPrefix(rel, ".."), "%q escaped base: %s", ref, rel)
	}
}

func TestBuildLookupScopes(t *testing.T) {
	t.Run("current first then default with legacy", func(t *testing.T) {
		scopes := buildLookupScopes("feature/x", "main", nil)
		require.Len(t, scopes, 2)
		assert.Equal(t, "feature/x", scopes[0].branch)
		assert.False(t, scopes[0].includeLegacy)
		assert.Equal(t, "main", scopes[1].branch)
		assert.True(t, scopes[1].includeLegacy)
	})

	t.Run("default branch current inherits legacy", func(t *testing.T) {
		scopes := buildLookupScopes("main", "main", nil)
		require.Len(t, scopes, 1)
		assert.Equal(t, "main", scopes[0].branch)
		assert.True(t, scopes[0].includeLegacy)
	})

	t.Run("fallback chain deduplicated", func(t *testing.T) {
		scopes := buildLookupScopes("dev", "main", []string{"release", "main", "release", "staging"})
		var names []string
		for _, s := range scopes {
			names = append(names, s.branch)
		}
		assert.Equal(t, []string{"dev", "main", "release", "staging"}, names)
		assert.False(t, scopes[3].includeLegacy)
	})

	t.Run("default fallback when default branch empty", func(t *testing.T) {
		scopes := buildLookupScopes("dev", "", nil)
		require.Len(t, scopes, 2)
		assert.Equal(t, "main", scopes[1].branch)
	})

	t.Run("detached still falls back", func(t *testing.T) {
		scopes := buildLookupScopes("", "master", nil)
		require.Len(t, scopes, 2)
		assert.Equal(t, DetachedHeadScope, scopes[0].branch)
		assert.Equal(t, "master", scopes[1].branch)
	})

	t.Run("matching", func(t *testing.T) {
		current := branchScope{branch: "feat"}
		fallback := branchScope{branch: "main", includeLegacy: true}
		flat := branchScope{all: true}
		assert.True(t, current.matches("feat"))
		assert.False(t, current.matches("main"))
		assert.False(t, current.matches(""))
		assert.True(t, fallback.matches("main"))
		assert.True(t, fallback.matches("")) // legacy entries
		assert.False(t, fallback.matches("feat"))
		assert.True(t, flat.matches("anything"))
		assert.True(t, flat.matches(""))
	})
}

func TestFindCache_BranchScopes(t *testing.T) {
	db, err := bolthold.Open(filepath.Join(t.TempDir(), "bolt.db"), 0o644, nil)
	require.NoError(t, err)
	defer db.Close()

	mustInsert := func(branch, key string, createdAt time.Time) {
		require.NoError(t, insertCache(db, &Cache{
			Branch:    branch,
			Key:       key,
			Version:   testCacheVersion,
			Size:      1,
			Complete:  true,
			UsedAt:    createdAt.Unix(),
			CreatedAt: createdAt.Unix(),
		}))
	}

	mainTime := time.Now().Add(-2 * time.Hour)
	featTime := time.Now().Add(-time.Hour)
	mustInsert("main", "deps-key", mainTime)
	mustInsert("feature/x", "deps-key-v2", featTime)
	mustInsert("", "legacy-key", mainTime) // old format entry

	scopes := buildLookupScopes("feature/x", "main", nil)

	t.Run("exact hit on current branch", func(t *testing.T) {
		cache, scope, err := findCache(db, []string{"deps-key-v2"}, testCacheVersion, scopes)
		require.NoError(t, err)
		require.NotNil(t, cache)
		assert.Equal(t, "feature/x", cache.Branch)
		assert.Equal(t, "feature/x", scope)
	})

	t.Run("current branch prefix preferred over default exact", func(t *testing.T) {
		cache, scope, err := findCache(db, []string{"deps-key"}, testCacheVersion, scopes)
		require.NoError(t, err)
		require.NotNil(t, cache)
		assert.Equal(t, "deps-key-v2", cache.Key)
		assert.Equal(t, "feature/x", scope)
	})

	t.Run("nothing matches anywhere", func(t *testing.T) {
		cache, _, err := findCache(db, []string{"other-key", "deps-key-zzz"}, testCacheVersion, scopes)
		require.NoError(t, err)
		assert.Nil(t, cache)
	})

	t.Run("legacy entry found via default branch", func(t *testing.T) {
		cache, scope, err := findCache(db, []string{"legacy-key"}, testCacheVersion, scopes)
		require.NoError(t, err)
		require.NotNil(t, cache)
		assert.Empty(t, cache.Branch)
		assert.Equal(t, "main", scope)
	})

	t.Run("flat mode finds every branch", func(t *testing.T) {
		cache, _, err := findCache(db, []string{"deps-key-v2"}, testCacheVersion, nil)
		require.NoError(t, err)
		require.NotNil(t, cache)
		assert.Equal(t, "feature/x", cache.Branch)
	})
}

type cacheFindResponse struct {
	Result          string `json:"result"`
	ArchiveLocation string `json:"archiveLocation"`
	CacheKey        string `json:"cacheKey"`
	CacheBranch     string `json:"cacheBranch"`
}

func startScopedHandler(t *testing.T, dir, branch, defaultBranch string, fallbacks ...string) *Handler {
	t.Helper()
	handler, err := StartHandlerWithOptions(Options{
		Dir:              dir,
		OutboundIP:       "127.0.0.1",
		Port:             0,
		BranchScope:      true,
		Branch:           branch,
		DefaultBranch:    defaultBranch,
		FallbackBranches: fallbacks,
	})
	require.NoError(t, err)
	return handler
}

func uploadScopedCache(t *testing.T, base, key string, content []byte) {
	t.Helper()
	var id uint64
	{
		body, err := json.Marshal(&Request{
			Key:     key,
			Version: testCacheVersion,
			Size:    int64(len(content)),
		})
		require.NoError(t, err)
		resp, err := http.Post(base+"/caches", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		got := struct {
			CacheID uint64 `json:"cacheId"`
		}{}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		id = got.CacheID
		resp.Body.Close()
	}
	{
		req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", 0, len(content)-1))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}
	{
		resp, err := http.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}
}

func findScopedCache(t *testing.T, base string, keys ...string) (int, cacheFindResponse) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, strings.Join(keys, ","), testCacheVersion))
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, cacheFindResponse{}
	}
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got cacheFindResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return resp.StatusCode, got
}

func downloadCache(t *testing.T, location string) []byte {
	t.Helper()
	resp, err := http.Get(location) //nolint:gosec
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	content, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return content
}

func TestHandler_BranchScope(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")

	mainContent := []byte("cache written on main")
	featContent := []byte("cache written on feature")

	// main writes a cache
	mainHandler := startScopedHandler(t, dir, "main", "main")
	mainBase := fmt.Sprintf("%s%s", mainHandler.ExternalURL(), apiPath)
	uploadScopedCache(t, mainBase, "shared-key", mainContent)
	uploadScopedCache(t, mainBase, "deps-node-v1", []byte("main deps"))
	require.NoError(t, mainHandler.Close())

	// feature branch run: own writes are isolated but main is reachable as fallback
	featHandler := startScopedHandler(t, dir, "feature/x", "main")
	featBase := fmt.Sprintf("%s%s", featHandler.ExternalURL(), apiPath)

	t.Run("exact fallback hit reports source branch", func(t *testing.T) {
		status, got := findScopedCache(t, featBase, "shared-key")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "shared-key", got.CacheKey)
		assert.Equal(t, "main", got.CacheBranch)
		assert.Equal(t, mainContent, downloadCache(t, got.ArchiveLocation))
	})

	t.Run("prefix fallback hit", func(t *testing.T) {
		status, got := findScopedCache(t, featBase, "deps-node-v2", "deps-node-")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "deps-node-v1", got.CacheKey)
		assert.Equal(t, "main", got.CacheBranch)
	})

	t.Run("unknown key misses", func(t *testing.T) {
		status, _ := findScopedCache(t, featBase, "does-not-exist")
		assert.Equal(t, http.StatusNoContent, status)
	})

	t.Run("same key on feature branch shadows only this run", func(t *testing.T) {
		uploadScopedCache(t, featBase, "shared-key", featContent)
		_, got := findScopedCache(t, featBase, "shared-key")
		assert.Equal(t, "feature/x", got.CacheBranch)
		assert.Equal(t, featContent, downloadCache(t, got.ArchiveLocation))
	})

	require.NoError(t, featHandler.Close())

	// main run again: its entry is untouched
	mainHandler2 := startScopedHandler(t, dir, "main", "main")
	defer mainHandler2.Close()
	mainBase2 := fmt.Sprintf("%s%s", mainHandler2.ExternalURL(), apiPath)
	_, got := findScopedCache(t, mainBase2, "shared-key")
	assert.Equal(t, "main", got.CacheBranch)
	assert.Equal(t, mainContent, downloadCache(t, got.ArchiveLocation))

	// another feature branch cannot see feature/x's private entry
	otherHandler := startScopedHandler(t, dir, "feature/y", "main")
	defer otherHandler.Close()
	otherBase := fmt.Sprintf("%s%s", otherHandler.ExternalURL(), apiPath)
	_, got = findScopedCache(t, otherBase, "shared-key")
	assert.Equal(t, "main", got.CacheBranch, "fallback must skip the other feature branch")
}

func TestHandler_BranchScope_FallbackChain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")

	releaseHandler := startScopedHandler(t, dir, "release", "main")
	releaseBase := fmt.Sprintf("%s%s", releaseHandler.ExternalURL(), apiPath)
	uploadScopedCache(t, releaseBase, "release-key", []byte("from release"))
	require.NoError(t, releaseHandler.Close())

	devHandler := startScopedHandler(t, dir, "dev", "main", "release")
	defer devHandler.Close()
	devBase := fmt.Sprintf("%s%s", devHandler.ExternalURL(), apiPath)

	_, got := findScopedCache(t, devBase, "release-key")
	assert.Equal(t, "release", got.CacheBranch)
}

func TestHandler_BranchScope_DetachedHead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")

	// empty branch simulates a detached HEAD / unresolvable reference
	detached := startScopedHandler(t, dir, "", "main")
	assert.Equal(t, DetachedHeadScope, detached.branch)
	detachedBase := fmt.Sprintf("%s%s", detached.ExternalURL(), apiPath)
	uploadScopedCache(t, detachedBase, "detached-key", []byte("detached"))

	db, err := detached.openDB()
	require.NoError(t, err)
	var caches []*Cache
	require.NoError(t, db.Find(&caches, bolthold.Where("Key").Eq("detached-key")))
	require.NoError(t, db.Close())
	require.Len(t, caches, 1)
	assert.Equal(t, DetachedHeadScope, caches[0].Branch)
	require.NoError(t, detached.Close())

	// another detached run still shares the placeholder scope
	detached2 := startScopedHandler(t, dir, "HEAD", "main")
	defer detached2.Close()
	base2 := fmt.Sprintf("%s%s", detached2.ExternalURL(), apiPath)
	_, got := findScopedCache(t, base2, "detached-key")
	assert.Equal(t, DetachedHeadScope, got.CacheBranch)

	// a feature branch cannot see the detached entry
	feat := startScopedHandler(t, dir, "feature/z", "main")
	defer feat.Close()
	featBase := fmt.Sprintf("%s%s", feat.ExternalURL(), apiPath)
	status, _ := findScopedCache(t, featBase, "detached-key")
	assert.Equal(t, http.StatusNoContent, status)
}

func TestHandler_BranchScope_LegacyEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")

	// old, flat handler writes a branchless entry
	flat, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	flatBase := fmt.Sprintf("%s%s", flat.ExternalURL(), apiPath)
	legacyContent := []byte("legacy cache")
	uploadScopedCache(t, flatBase, "legacy-key", legacyContent)

	db, err := flat.openDB()
	require.NoError(t, err)
	var caches []*Cache
	require.NoError(t, db.Find(&caches, bolthold.Where("Key").Eq("legacy-key")))
	require.Len(t, caches, 1)
	assert.Empty(t, caches[0].Branch, "flat mode must keep writing branchless entries")
	require.NoError(t, db.Close())
	require.NoError(t, flat.Close())

	// flat mode keeps hitting it
	flat2, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	flatBase2 := fmt.Sprintf("%s%s", flat2.ExternalURL(), apiPath)
	_, got := findScopedCache(t, flatBase2, "legacy-key")
	assert.Equal(t, "legacy-key", got.CacheKey)
	assert.Empty(t, got.CacheBranch, "flat responses must not carry the new field")
	assert.Equal(t, legacyContent, downloadCache(t, got.ArchiveLocation))
	require.NoError(t, flat2.Close())

	// scoped feature branch: legacy entry behaves like a default-branch entry
	feat := startScopedHandler(t, dir, "feature/legacy", "main")
	defer feat.Close()
	featBase := fmt.Sprintf("%s%s", feat.ExternalURL(), apiPath)
	_, got = findScopedCache(t, featBase, "legacy-key")
	assert.Equal(t, "main", got.CacheBranch)
	assert.Equal(t, legacyContent, downloadCache(t, got.ArchiveLocation))

	// scoped run on the default branch itself also hits the legacy entry
	main := startScopedHandler(t, dir, "main", "main")
	defer main.Close()
	mainBase := fmt.Sprintf("%s%s", main.ExternalURL(), apiPath)
	_, got = findScopedCache(t, mainBase, "legacy-key")
	assert.Equal(t, "main", got.CacheBranch)
}

func TestHandler_BranchScope_UnsafeBranchNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")

	content := []byte("unsafe")
	h := startScopedHandler(t, dir, "../../../evil/branch", "main")
	defer h.Close()
	assert.Equal(t, "evil/branch", h.branch)
	base := fmt.Sprintf("%s%s", h.ExternalURL(), apiPath)
	uploadScopedCache(t, base, "unsafe-key", content)
	_, got := findScopedCache(t, base, "unsafe-key")
	assert.Equal(t, h.branch, got.CacheBranch)
	assert.Equal(t, content, downloadCache(t, got.ArchiveLocation))

	// a different branch must not see the entry of the sanitized scope
	h2 := startScopedHandler(t, dir, "other/scope", "main")
	defer h2.Close()
	base2 := fmt.Sprintf("%s%s", h2.ExternalURL(), apiPath)
	status, _ := findScopedCache(t, base2, "unsafe-key")
	assert.Equal(t, http.StatusNoContent, status)

	// artifacts on disk are stored by numeric id only: no branch name escapes
	// into storage paths.
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		assert.NotContains(t, path, "evil")
		return nil
	})
	require.NoError(t, err)
}

func TestHandler_gcCache_BranchScoped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	h := startScopedHandler(t, dir, "main", "main")
	defer h.Close()

	now := time.Now()
	old := func(branch, key string, createdAt time.Time, usedAt time.Time) *Cache {
		return &Cache{
			Branch:    branch,
			Key:       key,
			Version:   "v",
			Complete:  true,
			UsedAt:    usedAt.Unix(),
			CreatedAt: createdAt.Unix(),
		}
	}

	cases := []struct {
		name  string
		cache *Cache
		kept  bool
	}{
		// same key/version on two branches: only the older copy of each is removed
		{name: "a-old", cache: old("a", "dup", now.Add(-3*time.Hour), now.Add(-(keepOld + time.Second))), kept: false},
		{name: "a-new", cache: old("a", "dup", now.Add(-time.Hour), now), kept: true},
		{name: "b-old", cache: old("b", "dup", now.Add(-4*time.Hour), now.Add(-(keepOld + time.Second))), kept: false},
		{name: "b-new", cache: old("b", "dup", now.Add(-2*time.Hour), now), kept: true},
		// legacy entry is deduped together with the default-branch entries
		{name: "legacy-old", cache: old("", "legacy", now.Add(-3*time.Hour), now.Add(-(keepOld + time.Second))), kept: false},
		{name: "main-new", cache: old("main", "legacy", now.Add(-time.Hour), now), kept: true},
		// old but recently used: kept to not break in-flight downloads
		{name: "c-old-recently-used", cache: old("c", "dup2", now.Add(-2*time.Hour), now.Add(-time.Minute)), kept: true},
		{name: "c-new", cache: old("c", "dup2", now.Add(-time.Hour), now), kept: true},
	}

	db, err := h.openDB()
	require.NoError(t, err)
	for _, tc := range cases {
		require.NoError(t, insertCache(db, tc.cache))
	}
	require.NoError(t, db.Close())

	h.gcAt = time.Time{} // ensure gcCache will not skip
	h.gcCache()

	db, err = h.openDB()
	require.NoError(t, err)
	defer db.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := &Cache{}
			err := db.Get(tc.cache.ID, cache)
			if tc.kept {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, bolthold.ErrNotFound)
			}
		})
	}
}
