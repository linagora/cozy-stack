package vfs

// SetDirSizeBatchForTest sets how many subdirectories DirSize reads from
// CouchDB at a time. It returns a function restoring the previous value.
func SetDirSizeBatchForTest(n int) func() {
	old := dirSizeBatch
	dirSizeBatch = n
	return func() { dirSizeBatch = old }
}
