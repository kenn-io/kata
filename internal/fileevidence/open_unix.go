//go:build unix

package fileevidence

import (
	"os"
	"syscall"
)

// Nonblocking open prevents a regular-to-FIFO replacement from hanging before
// descriptor type validation. NOFOLLOW also rejects a swapped final symlink.
func openLocalFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
