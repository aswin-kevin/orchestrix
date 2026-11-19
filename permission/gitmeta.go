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
//     new one cannot be planted anywhere, under any name, or
//   - it is a config file git reads for every repository: the global
//     .gitconfig, the system gitconfig, or a git/config file, as in
//     ~/.config/git/config or any $XDG_CONFIG_HOME/git/config. These matter
//     when the workspace holds a home folder.
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
	for i, c := range components {
		components[i] = fileName(c)
		if c := components[i]; c == ".git" || c == "git~1" || strings.HasSuffix(c, ".git") {
			return true
		}
	}
	switch base := components[len(components)-1]; base {
	case "head", ".gitconfig", "gitconfig":
		return true
	case "config":
		return len(components) > 1 && components[len(components)-2] == "git"
	}
	return false
}

// fileName returns a path component as a case-insensitive file system that
// ignores trailing dots and spaces, as Windows does, sees it.
func fileName(component string) string {
	return strings.ToLower(strings.TrimRight(component, ". "))
}
