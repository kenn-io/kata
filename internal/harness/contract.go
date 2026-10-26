package harness

import "strings"

// bindModelContract keeps inherited model data tied to its exact native
// harness/session identity. Native models retain their native provider too.
func bindModelContract(env Env, detection Detection) Detection {
	if detection.Model != "" || value(env, "KATA_HARNESS_SESSION") != detection.Harness+":"+detection.SessionID {
		return detection
	}
	model := value(env, "KATA_HARNESS_MODEL")
	if provider, remainder, found := strings.Cut(model, "/"); found {
		detection.Provider, detection.Model = provider, remainder
	} else {
		detection.Provider, detection.Model = "", model
	}
	return detection
}
