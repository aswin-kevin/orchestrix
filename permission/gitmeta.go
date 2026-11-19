package permission

import "strings"

// gitMetadataWrite returns the first path in paths that writes git metadata,
// if any. Git runs commands named in a repository's config (core.fsmonitor,
// aliases, hooks, filters), so a write there would let an allowed git
// command, even "git status", run anything. A path is git metadata when
//
//   - one of its components is a .git directory or file, or a directory
//     named like a bare repository (repo.git), or
//   - it is a HEAD file, without which no directory is a repository, so a
//     new one cannot be planted anywhere, under any name.
//
// Names are compared as a case-insensitive file system sees them, ignoring
// the trailing dots and spaces and the short name (git~1) Windows accepts.
func gitMetadataWrite(paths []PathAccess) (string, bool) {
	for _, p := range paths {
		if p.Op == OpWrite && isGitMetadata(p.Path) {
			return p.Path, true
		}
	}
	return "", false
}

func isGitMetadata(p string) bool {
	components := strings.Split(p, "/")
	for _, c := range components {
		name := strings.ToLower(strings.TrimRight(c, ". "))
		if name == ".git" || name == "git~1" || strings.HasSuffix(name, ".git") {
			return true
		}
	}
	return strings.EqualFold(strings.TrimRight(components[len(components)-1], ". "), "HEAD")
}
