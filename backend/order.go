package backend

import (
	"slices"
	"strings"
)

// ComparePaths orders virtual paths the way a directory walk visits them:
// segment by segment, so "/a/x" sorts before "/a.txt" (as "a" < "a.txt").
func ComparePaths(a, b string) int {
	return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
}

// SortGlobResults orders infos newest first, breaking ties by path so results
// are deterministic.
func SortGlobResults(infos []FileInfo) {
	slices.SortFunc(infos, func(a, b FileInfo) int {
		if c := b.ModTime.Compare(a.ModTime); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
}
