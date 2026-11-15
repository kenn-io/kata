//go:build darwin

#include "textflag.h"

// Trampolines into the dynamically imported libSystem functions. The same
// source assembles for amd64 and arm64.

TEXT libxpc_launch_activate_socket_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libxpc_launch_activate_socket(SB)
GLOBL	·launchActivateSocketTrampolineAddr(SB), RODATA, $8
DATA	·launchActivateSocketTrampolineAddr(SB)/8, $libxpc_launch_activate_socket_trampoline<>(SB)

TEXT libc_free_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_free(SB)
GLOBL	·freeTrampolineAddr(SB), RODATA, $8
DATA	·freeTrampolineAddr(SB)/8, $libc_free_trampoline<>(SB)
