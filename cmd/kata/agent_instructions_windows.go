package main

import "io/fs"

// singleLinkFile reports false because FileInfo carries no link count on
// Windows, which keeps the in-place fallback off there.
func singleLinkFile(fs.FileInfo) bool { return false }
