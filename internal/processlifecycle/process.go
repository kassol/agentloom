// Package processlifecycle provides the small set of process operations used
// by CodexLoom's local service and reloader.
package processlifecycle

import "fmt"

func shutdownEventName(pid int) string {
	return fmt.Sprintf(`Local\CodexLoomShutdown-%d`, pid)
}
