package federationcoord

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// RelayRootTransitionPath validates the complete signed replacement path
// before any pin changes. The existing key retains authority over transitions.
func RelayRootTransitionPath(pin, advertised db.RootKeyPin, history []db.RootKeyTransition) ([]db.RootKeyTransition, error) {
	if err := db.ValidateRootKeyPin(advertised); err != nil {
		return nil, err
	}
	if advertised.Retired || advertised.ProjectUID != pin.ProjectUID || advertised.AuthorityUID != pin.AuthorityUID {
		return nil, errors.New("relay root key changes pinned authority")
	}
	byPrevious := make(map[string]db.RootKeyTransition, len(history))
	for _, transition := range history {
		if transition.Next.ProjectUID != pin.ProjectUID || transition.Next.AuthorityUID != pin.AuthorityUID || transition.Next.Retired {
			return nil, errors.New("relay root transition changes pinned authority")
		}
		if _, exists := byPrevious[transition.PreviousKeyID]; exists {
			return nil, errors.New("relay root transition history branches")
		}
		byPrevious[transition.PreviousKeyID] = transition
	}
	var path []db.RootKeyTransition
	visited := make(map[string]bool)
	for pin.KeyID != advertised.KeyID {
		transition, exists := byPrevious[pin.KeyID]
		if !exists || visited[pin.KeyID] {
			return nil, errors.New("relay root replacement lacks a signed transition path")
		}
		if err := db.VerifyRootKeyTransition(pin, transition); err != nil {
			return nil, err
		}
		visited[pin.KeyID] = true
		path = append(path, transition)
		pin = transition.Next
	}
	if !slices.Equal(pin.PublicKey, advertised.PublicKey) {
		return nil, errors.New("relay root key differs from signed replacement")
	}
	return path, nil
}

func relayEnrollmentPath(ctx context.Context, store db.Storage, handshake api.RelayHandshake) (db.RootKeyPin, bool, []db.RootKeyTransition, error) {
	if handshake.ProtocolVersion != db.RelayProtocolVersion {
		return db.RootKeyPin{}, false, nil, errors.New("root enrollment requires the supported relay protocol")
	}
	current, err := store.RootAuthority(ctx, handshake.Root.ProjectUID)
	fresh := errors.Is(err, db.ErrNotFound)
	if err != nil && !fresh {
		return db.RootKeyPin{}, false, nil, err
	}
	if fresh {
		current = handshake.Root
		if handshake.EnrollmentRoot != nil {
			current = *handshake.EnrollmentRoot
			current.Retired = false
		}
	}
	transitions, err := RelayRootTransitionPath(current, handshake.Root, handshake.RootKeyTransitions)
	return current, fresh, transitions, err
}

// ValidateRelayRootAuthority checks enrollment without installing a project or
// key. Initial setup uses this before changing local persisted state.
func ValidateRelayRootAuthority(ctx context.Context, store db.Storage, handshake api.RelayHandshake) error {
	_, _, _, err := relayEnrollmentPath(ctx, store, handshake)
	return err
}

// PinRelayRootAuthority reuses native pin/rotation transactions after validating
// the entire chain. A subsequent handshake cannot replace known key history.
func PinRelayRootAuthority(ctx context.Context, store db.Storage, handshake api.RelayHandshake) error {
	current, fresh, transitions, err := relayEnrollmentPath(ctx, store, handshake)
	if err != nil {
		return err
	}
	if fresh {
		if err := store.PinRootAuthority(ctx, current); err != nil {
			return err
		}
	}
	for _, transition := range transitions {
		if err := store.RotateRootAuthority(ctx, transition); err != nil {
			return err
		}
	}
	return nil
}
