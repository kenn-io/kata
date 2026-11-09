// Package linkpath resolves a file path the way the OS does when it opens it.
package linkpath

import (
	"os"
	"path/filepath"
	"strings"
)

// maxHops bounds a symlink chain, as the OS does when it opens one.
const maxHops = 40

// Chain lists path and every symlink hop the OS follows to open it. Each name
// has its directory part resolved in filesystem order, so a ".." after a
// directory symlink applies to that link's target, as it does for the OS.
// filepath.Join and filepath.Dir would collapse it as text first. The last
// name is the file the OS opens; it may not exist yet.
func Chain(path string) []string {
	name := resolveParent(path)
	names := []string{name}
	for range maxHops {
		target, err := os.Readlink(name)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Dir(name) + string(filepath.Separator) + target
		}
		name = resolveParent(target)
		names = append(names, name)
	}
	return names
}

// resolveParent resolves everything before path's last name and keeps that
// name as written, since it may be a symlink or not exist yet.
func resolveParent(path string) string {
	i := strings.LastIndexAny(path, `/`+string(filepath.Separator))
	if i < 0 {
		return path
	}
	parent, base := path[:i], path[i+1:]
	if base == "" || base == "." || base == ".." {
		return filepath.Clean(path)
	}
	if parent == filepath.VolumeName(path) {
		parent += string(filepath.Separator)
	}
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(resolved, base)
	}
	return filepath.Clean(path)
}
