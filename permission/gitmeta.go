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
//   - it is a config file git reads for every repository, or one such files
//     commonly include: the global .gitconfig and files named like it
//     (.gitconfig.local, .gitconfig-work), the system gitconfig, anything in
//     a .config/git folder, or a git/config file, as in any
//     $XDG_CONFIG_HOME/git/config. These matter when the workspace holds a
//     home folder. A file included under some other name is not covered.
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
	base := components[len(components)-1]
	parent := func(n int) string { // the nth folder up from the file, or ""
		if i := len(components) - 1 - n; i >= 0 {
			return components[i]
		}
		return ""
	}
	switch {
	case base == "head", base == "gitconfig", strings.HasPrefix(base, ".gitconfig"):
		return true
	case parent(1) == "git" && (base == "config" || parent(2) == ".config"):
		return true
	}
	return false
}

// fileName returns a path component as a case-insensitive file system that
// ignores trailing dots and spaces, as Windows does, sees it.
func fileName(component string) string {
	return strings.ToLower(strings.TrimRight(component, ". "))
}
