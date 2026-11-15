package daemon

import (
	"context"
	"slices"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func addFederationNativeLeavePreparation(
	credential config.FederationCredential,
	credentialFound bool,
	binding db.FederationBinding,
	preparationID, transitionKey string,
	originalEnabled, originalPushEnabled bool,
) config.FederationCredential {
	if !credentialFound {
		credential = config.FederationCredential{
			HubURL: binding.HubURL, HubProjectID: binding.HubProjectID,
		}
	}
	state := cloneFederationNativeLeaveState(credential.NativeLeave)
	if state == nil {
		state = &config.FederationNativeLeaveState{
			Enabled: originalEnabled, PushEnabled: originalPushEnabled,
			CreatedCredential: !credentialFound,
		}
	}
	var preparation *config.FederationNativeLeavePreparation
	for i := range state.Preparations {
		if state.Preparations[i].ID == preparationID {
			preparation = &state.Preparations[i]
			break
		}
	}
	if preparation == nil {
		state.Preparations = append(state.Preparations, config.FederationNativeLeavePreparation{ID: preparationID})
		preparation = &state.Preparations[len(state.Preparations)-1]
	}
	if !slices.Contains(preparation.Keys, transitionKey) {
		preparation.Keys = append(preparation.Keys, transitionKey)
	}
	credential.NativeLeave = state
	return credential
}

func removeFederationNativeLeavePreparation(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	projectUID string,
	credential config.FederationCredential,
	preparationID string,
) (config.FederationCredential, config.FederationNativeLeavePreparation, bool, bool, error) {
	state := cloneFederationNativeLeaveState(credential.NativeLeave)
	if state == nil {
		return credential, config.FederationNativeLeavePreparation{}, false, false, nil
	}
	index := -1
	for i := range state.Preparations {
		if state.Preparations[i].ID == preparationID {
			index = i
			break
		}
	}
	if index < 0 {
		return credential, config.FederationNativeLeavePreparation{}, false, false, nil
	}
	removed := state.Preparations[index]
	state.Preparations = slices.Delete(state.Preparations, index, index+1)
	final := len(state.Preparations) == 0
	replacement := credential
	if final {
		replacement.NativeLeave = nil
		if state.CreatedCredential {
			if remover, ok := credentials.(config.FederationCredentialRemover); ok {
				if err := remover.DeleteFederationCredentialIfUnchanged(ctx, projectUID, credential); err != nil {
					return credential, config.FederationNativeLeavePreparation{}, false, false, err
				}
			} else {
				current, found, err := credentials.FederationCredential(ctx, projectUID)
				if err != nil {
					return credential, config.FederationNativeLeavePreparation{}, false, false, err
				}
				if !found || !current.Equal(credential) {
					return credential, config.FederationNativeLeavePreparation{}, false, false, config.ErrFederationCredentialConflict
				}
				if err := credentials.DeleteFederationCredential(ctx, projectUID); err != nil {
					return credential, config.FederationNativeLeavePreparation{}, false, false, err
				}
			}
			return replacement, removed, true, true, nil
		}
	} else {
		replacement.NativeLeave = state
	}
	if err := replaceFederationCredential(ctx, credentials, projectUID, credential, true, replacement); err != nil {
		return credential, config.FederationNativeLeavePreparation{}, false, false, err
	}
	return replacement, removed, true, final, nil
}

func cloneFederationNativeLeaveState(state *config.FederationNativeLeaveState) *config.FederationNativeLeaveState {
	if state == nil {
		return nil
	}
	clone := *state
	clone.Preparations = make([]config.FederationNativeLeavePreparation, len(state.Preparations))
	for i, preparation := range state.Preparations {
		clone.Preparations[i] = preparation
		clone.Preparations[i].Keys = slices.Clone(preparation.Keys)
	}
	return &clone
}

func federationNativeLeaveHasPreparation(state *config.FederationNativeLeaveState, preparationID string) bool {
	if state == nil {
		return false
	}
	for _, preparation := range state.Preparations {
		if preparation.ID == preparationID {
			return true
		}
	}
	return false
}

func replaceFederationCredential(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	projectUID string,
	expected config.FederationCredential,
	expectedFound bool,
	replacement config.FederationCredential,
) error {
	if expectedFound {
		if replacer, ok := credentials.(config.FederationCredentialReplacer); ok {
			return replacer.ReplaceFederationCredential(ctx, config.FederationCredentialReplacement{
				ProjectUID: projectUID, Expected: expected, Replacement: replacement,
			})
		}
		current, found, err := credentials.FederationCredential(ctx, projectUID)
		if err != nil {
			return err
		}
		if !found || !current.Equal(expected) {
			return config.ErrFederationCredentialConflict
		}
	}
	return credentials.StoreFederationCredential(ctx, projectUID, replacement)
}
