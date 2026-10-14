package artifactcache

import (
	"regexp"
	"strings"
)

const (
	refsHeadsPrefix = "refs/heads/"

	// DetachedHeadScope is the recognizable placeholder scope used when the
	// current branch cannot be determined (detached HEAD, a tag, a pull
	// request merge ref or a non-git working directory). Runs in this scope
	// are isolated from every branch but can still fall back to the
	// default branch.
	DetachedHeadScope = "(detached)"

	// defaultFallbackBranch is used as the fallback scope when neither the
	// default branch nor a fallback chain is configured.
	defaultFallbackBranch = "main"
)

// unsafeBranchChars matches characters that are not allowed in a git branch
// name or that would be dangerous in identifiers: ASCII control chars
// (including NUL), whitespace and the git refname specials "~ ^ : ? * [".
var unsafeBranchChars = regexp.MustCompile(`[\x00-\x20\x7f~^:?*\[]+`)

// NormalizeBranch turns a run-side git reference into a branch scope name.
//
// It accepts both a plain branch name ("feature/x") and a full reference
// ("refs/heads/feature/x"). When the reference is empty, "HEAD", or points at
// a non-branch ref (tag, pull request, ...), the recognizable DetachedHeadScope
// is returned. The returned name never contains path traversal segments or
// unsafe characters, so it cannot corrupt storage paths or database keys
// (cache artifacts themselves are still stored by numeric id only).
func NormalizeBranch(ref string) string {
	name := strings.TrimSpace(ref)
	switch {
	case name == "" || name == "HEAD":
		return DetachedHeadScope
	case strings.HasPrefix(name, refsHeadsPrefix):
		name = name[len(refsHeadsPrefix):]
	case strings.HasPrefix(name, "refs/"):
		// tags, pull request refs and other non-branch refs
		return DetachedHeadScope
	}

	name = sanitizeBranchName(name)
	if name == "" {
		return DetachedHeadScope
	}
	return name
}

// sanitizeBranchName makes a branch name safe while preserving its hierarchy:
// "/" (a legal branch name character, e.g. "feature/x") is kept, while
// traversal segments and characters that are unsafe for paths and keys are
// removed.
func sanitizeBranchName(name string) string {
	// Normalize Windows-style path separators so "feature\login" is handled as
	// the same hierarchy as "feature/login".
	name = strings.ReplaceAll(name, "\\", "/")
	// ".." is used for path traversal and invalid in git ref names; "@{..."
	// is a git revision syntax.
	name = strings.ReplaceAll(name, "..", "--")
	name = strings.ReplaceAll(name, "@{", "--")
	name = unsafeBranchChars.ReplaceAllString(name, "-")

	var segments []string
	for _, seg := range strings.Split(name, "/") {
		// Drop empty segments (leading, trailing and duplicated slashes),
		// current-directory segments and edge dots (which are invalid at the
		// edges of a git ref component).
		seg = strings.Trim(seg, ".")
		if seg == "" {
			continue
		}
		// a leading dash would be parsed as a command line option by git
		seg = strings.TrimLeft(seg, "-")
		if seg == "" {
			continue
		}
		if strings.HasSuffix(seg, ".lock") {
			seg = strings.TrimSuffix(seg, ".lock") + "-lock"
		}
		segments = append(segments, seg)
	}
	if len(segments) == 0 {
		return ""
	}
	return strings.Join(segments, "/")
}

// branchScope describes one lookup scope. An entry matches when its recorded
// branch equals branch; includeLegacy additionally matches entries without a
// recorded branch (written by older act versions or in flat mode), which are
// treated as entries of the default branch. The all scope matches every
// entry and is used in flat mode to preserve the historical behavior.
type branchScope struct {
	branch        string
	includeLegacy bool
	all           bool
}

func (s branchScope) matches(branch string) bool {
	if s.all {
		return true
	}
	if branch == s.branch {
		return true
	}
	return s.includeLegacy && branch == ""
}

// buildLookupScopes returns the ordered scopes for a lookup: the current
// branch first, followed by the default branch (which inherits legacy
// entries) and the remaining configurable fallback branches, deduplicated.
func buildLookupScopes(current, defaultBranch string, fallbacks []string) []branchScope {
	current = NormalizeBranch(current)
	if strings.TrimSpace(defaultBranch) == "" {
		defaultBranch = defaultFallbackBranch
	} else {
		defaultBranch = NormalizeBranch(defaultBranch)
	}

	scopes := []branchScope{{
		branch:        current,
		includeLegacy: current == defaultBranch,
	}}
	seen := map[string]bool{current: true}

	add := func(branch string, includeLegacy bool) {
		if branch == "" || seen[branch] {
			return
		}
		seen[branch] = true
		scopes = append(scopes, branchScope{
			branch:        branch,
			includeLegacy: includeLegacy,
		})
	}

	// The default branch is always the first fallback, even when the caller
	// also configures an explicit fallback chain.
	add(defaultBranch, true)
	for _, fallback := range fallbacks {
		add(NormalizeBranch(fallback), false)
	}
	return scopes
}
