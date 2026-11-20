package disk

import "os"

// SetTempCreatedHook installs fn to run on each temporary file replaceFile
// creates, right after it is created; the returned func restores the default.
func SetTempCreatedHook(fn func(*os.File)) (restore func()) {
	old := tempCreated
	tempCreated = fn
	return func() { tempCreated = old }
}
