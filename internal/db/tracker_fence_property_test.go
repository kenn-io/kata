package db_test

import (
	"errors"
	"fmt"
	"testing"

	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

func TestTrackerUnknownFenceNeverAdvancesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		previous := trackerFenceFixture()
		previous.Phase = db.TrackerWriteUnknown
		next := previous
		next.Sequence++
		next.PreviousHead = db.TrackerFenceHead(previous)
		next.Locator = fmt.Sprintf("other-item-%d", hegel.Draw(ht, hegel.Integers(0, 1000000)))
		var err error
		next.WriteUID, err = db.TrackerStatusWriteUID(next.RootInstanceUID, next.ProjectUID, next.SourceKey, next.Locator, next.PendingEventUID, next.Target)
		if err != nil {
			ht.Fatal(err)
		}
		next.Phase = db.TrackerWritePrepared
		if !errors.Is(db.ValidateTrackerFenceTransition(previous, next), db.ErrTrackerOutcomeUnknown) {
			ht.Fatal("unknown attempt was replaced by a fresh sequence")
		}
		settled := previous
		settled.Phase = db.TrackerWriteNotSent
		settled.Result = []byte(`{"not_sent":true}`)
		next.PreviousHead = db.TrackerFenceHead(settled)
		if err := db.ValidateTrackerFenceTransition(settled, next); err != nil {
			ht.Fatal(err)
		}
	}, hegel.WithTestCases(1000))
}

func TestTrackerWriteIdentitySeparatesSourceAndLocatorProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		f := trackerFenceFixture()
		a := fmt.Sprintf("source-%d", hegel.Draw(ht, hegel.Integers(0, 1000000)))
		b := fmt.Sprintf("item-%d", hegel.Draw(ht, hegel.Integers(0, 1000000)))
		first, err := db.TrackerStatusWriteUID(f.RootInstanceUID, f.ProjectUID, a+"."+b, "x", f.PendingEventUID, f.Target)
		if err != nil {
			ht.Fatal(err)
		}
		second, err := db.TrackerStatusWriteUID(f.RootInstanceUID, f.ProjectUID, a, b+".x", f.PendingEventUID, f.Target)
		if err != nil {
			ht.Fatal(err)
		}
		if first == second {
			ht.Fatal("distinct component boundaries produced the same write identity")
		}
	}, hegel.WithTestCases(1000))
}

// Only a same-process non-send can advance the identical semantic intent.
func TestTrackerSameIntentRetryRequiresProvenNonSendProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		f := trackerFenceFixture()
		f.Locator = fmt.Sprintf("item-%d", hegel.Draw(ht, hegel.Integers(0, 1000000)))
		f.WriteUID, _ = db.TrackerStatusWriteUID(f.RootInstanceUID, f.ProjectUID, f.SourceKey, f.Locator, f.PendingEventUID, f.Target)
		f.Phase = db.TrackerWriteNotSent
		f.Result = []byte(`{"not_sent":true}`)
		next := f
		next.Sequence++
		next.PreviousHead = db.TrackerFenceHead(f)
		next.Phase = db.TrackerWritePrepared
		next.Result = nil
		if err := db.ValidateTrackerFenceTransition(f, next); err != nil {
			ht.Fatal(err)
		}
		invalid := next
		switch hegel.Draw(ht, hegel.Integers(0, 2)) {
		case 0:
			invalid.Connection.Generation = "different"
		case 1:
			invalid.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
		case 2:
			invalid.ActivationRequired = true
		}
		if err := invalid.Validate(); err != nil {
			ht.Fatal(err)
		}
		if db.ValidateTrackerFenceTransition(f, invalid) == nil {
			ht.Fatal("retry crossed a connection, process, or activation boundary")
		}
		for _, phase := range []db.TrackerWritePhase{db.TrackerWriteUnknown, db.TrackerWriteConfirmed, db.TrackerWriteAcknowledged} {
			prior := f
			prior.Phase = phase
			prior.ProviderAcknowledged = phase != db.TrackerWriteUnknown
			if phase == db.TrackerWriteUnknown {
				prior.Result = nil
			}
			next.PreviousHead = db.TrackerFenceHead(prior)
			if db.ValidateTrackerFenceTransition(prior, next) == nil {
				ht.Fatal("unproven or acknowledged attempt admitted a duplicate")
			}
		}
	}, hegel.WithTestCases(1000))
}
