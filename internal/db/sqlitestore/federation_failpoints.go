package sqlitestore

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func federationFailpoint(name string) error {
	spec := strings.TrimSpace(os.Getenv("KATA_TEST_FEDERATION_FAILPOINTS"))
	if spec == "" {
		return nil
	}
	for _, entry := range strings.Split(spec, ",") {
		key, action, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		return runFederationFailpointAction(name, strings.TrimSpace(action))
	}
	return nil
}

func runFederationFailpointAction(name, action string) error {
	switch {
	case action == "exit":
		_, _ = fmt.Fprintf(os.Stderr, "kata federation failpoint %s: exit\n", name)
		_ = os.Stderr.Sync()
		os.Exit(23)
	case strings.HasPrefix(action, "sleep:"):
		d, err := time.ParseDuration(strings.TrimPrefix(action, "sleep:"))
		if err != nil {
			return fmt.Errorf("federation failpoint %s: parse sleep: %w", name, err)
		}
		time.Sleep(d)
		return nil
	default:
		return fmt.Errorf("federation failpoint %s: unknown action %q", name, action)
	}
	return nil
}
