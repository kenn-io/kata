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
