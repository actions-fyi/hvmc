//go:build !windows

package fileacl

// SecureDir does nothing outside Windows: the directory's permission bits
// apply.
func SecureDir(string) error { return nil }

// SecureConfig does nothing outside Windows: the file's permission bits
// apply.
func SecureConfig(string) error { return nil }
