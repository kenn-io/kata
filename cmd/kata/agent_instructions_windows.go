package main

import "io/fs"

// openNoFollow is unused on Windows, where the in-place fallback is off.
const openNoFollow = 0

// singleLinkFile reports false because FileInfo carries no link count on
// Windows, which keeps the in-place fallback off there.
func singleLinkFile(fs.FileInfo) bool { return false }
