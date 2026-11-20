// Package daemonadvice formats recovery instructions for Kata's user interfaces.
package daemonadvice

import "go.kenn.io/kata/internal/client"

// Recovery returns the recovery advice with a shell-quoted profile command.
func Recovery(e *client.LocalDaemonUnreachableError) string {
	return e.RecoveryAdvice()
}
