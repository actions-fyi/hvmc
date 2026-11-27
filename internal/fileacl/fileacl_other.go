//go:build !windows

package fileacl

import "os"

// MkdirSecure creates the state directory at path (and missing parents)
// with mode 0700; an existing directory is left as it is.
func MkdirSecure(path string) error { return os.MkdirAll(path, 0o700) }

// SecureDir does nothing outside Windows: the directory's permission bits
// apply.
func SecureDir(string) error { return nil }

// SecureConfig does nothing outside Windows: the file's permission bits
// apply.
func SecureConfig(string) error { return nil }
