// Package federationcoord serializes endpoint changes with project-scoped
// federation transport work inside one daemon process.
package federationcoord

import (
	"strconv"
	"sync"
)

var projectGates sync.Map

// Key identifies one project in one durable daemon instance.
func Key(instanceUID string, projectID int64) string {
	return instanceUID + "\x00" + strconv.FormatInt(projectID, 10)
}

// BeginSync admits one shared transport operation. The returned function must
// be called when all remote responses and local commits are complete.
func BeginSync(key string) func() {
	gate := gateFor(key)
	gate.RLock()
	return gate.RUnlock
}

// BeginRebind drains existing transport operations and prevents new ones from
// starting until the returned function is called.
func BeginRebind(key string) func() {
	gate := gateFor(key)
	gate.Lock()
	return gate.Unlock
}

func gateFor(key string) *sync.RWMutex {
	gate, _ := projectGates.LoadOrStore(key, &sync.RWMutex{})
	return gate.(*sync.RWMutex)
}
