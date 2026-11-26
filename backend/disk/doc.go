// Package disk provides a backend.Backend over a directory on the real file
// system. It is a security boundary: every path is resolved through symlinks
// and must stay inside the root, otherwise the call fails with
// backend.ErrOutsideRoot (or, in directory walks, the entry is skipped).
//
// Threat model: hard links are not detected, so a file inside the root that
// was already hard-linked to a file outside it can be read (writes are safe,
// because they replace the file by rename instead of writing through the
// link). Do not point the backend at a directory an untrusted party can
// pre-populate with hard links.
//
// Races: every path is checked up front, which yields ErrOutsideRoot, and
// every file operation then goes through an os.Root opened on the root
// directory, which refuses to leave it at the moment of the call. So a
// concurrent writer, such as a background shell job, that swaps a directory
// for a symlink after the check can make an operation fail but never redirect
// it outside the root.
package disk
