//go:build darwin

package launchd

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// syscallSyscall is the runtime's libc call bridge, the one
// golang.org/x/sys/unix also uses on Darwin. The runtime exports it for this
// purpose, so calling a libSystem function needs no cgo.
//
//go:linkname syscallSyscall syscall.syscall
func syscallSyscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:cgo_import_dynamic libxpc_launch_activate_socket launch_activate_socket "/usr/lib/system/libxpc.dylib"
//go:cgo_import_dynamic libc_free free "/usr/lib/libSystem.B.dylib"

// Set in launchd_darwin.s.
var (
	launchActivateSocketTrampolineAddr uintptr
	freeTrampolineAddr                 uintptr
)

// Files returns the descriptors launchd created for the Sockets entry name.
// The caller owns and must close them.
func Files(name string) ([]*os.File, error) {
	cname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}
	var fds *int32
	var count uintptr
	res, _, _ := syscallSyscall(launchActivateSocketTrampolineAddr,
		uintptr(unsafe.Pointer(cname)), uintptr(unsafe.Pointer(&fds)), uintptr(unsafe.Pointer(&count))) //nolint:gosec // G103: libc call with out-parameters, as in x/sys/unix.
	runtime.KeepAlive(cname)
	if res != 0 {
		return nil, fmt.Errorf("launch_activate_socket %q: %w", name, syscall.Errno(res))
	}
	if fds == nil {
		return nil, nil
	}
	defer func() {
		// free(3) returns nothing.
		_, _, _ = syscallSyscall(freeTrampolineAddr, uintptr(unsafe.Pointer(fds)), 0, 0) //nolint:gosec // G103: releases launchd's malloc'd array.
	}()
	files := make([]*os.File, 0, count)
	for _, fd := range unsafe.Slice(fds, count) { //nolint:gosec // G103: launchd returns count descriptors at fds.
		if fd < 0 {
			for _, file := range files {
				_ = file.Close()
			}
			return nil, fmt.Errorf("launch_activate_socket %q: invalid descriptor %d", name, fd)
		}
		files = append(files, os.NewFile(uintptr(fd), name))
	}
	return files, nil
}
