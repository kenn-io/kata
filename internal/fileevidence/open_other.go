//go:build !unix

package fileevidence

import "os"

func openLocalFile(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
