package kv

import (
	"errors"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, which syscall does not
// name.
const errSharingViolation syscall.Errno = 32

// isTransient reports an error Windows returns while another handle has the
// file open: os.Open shares files for reading and writing but not deleting,
// so a rename over an open key file, or an open of a file a rename is
// replacing, fails with a sharing violation or "Access is denied" until the
// other handle is closed.
func isTransient(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED || errno == errSharingViolation
}
