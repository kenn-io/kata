package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
)

// A pending request's identity is its immutable issue/project pair and the
// complete decoded notify value, independent of mutable presentation fields.
func TestInboxWatchSuppressesUnchangedRequests(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		first, seen := inboxWatchChanges(nil, []inboxWatchRequest{request, request})
		require.Equal(ht, []inboxWatchRequest{request}, first)
		request.Title = inboxDrawText(ht)
		request.Ref = inboxDrawText(ht)
		second, _ := inboxWatchChanges(seen, []inboxWatchRequest{request})
		require.Empty(ht, second)
	})
}

func TestInboxWatchReemitsChangedNotificationFields(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		_, seen := inboxWatchChanges(nil, []inboxWatchRequest{request})
		switch inboxDrawField(ht) {
		case 0:
			request.value.From += "changed"
		case 1:
			request.value.Teammate += "changed"
		case 2:
			request.value.Message += "changed"
		case 3:
			request.value.Re += "changed"
		case 4:
			request.value.Kind += "changed"
		case 5:
			request.value.Broadcast = !request.value.Broadcast
		}
		changed, _ := inboxWatchChanges(seen, []inboxWatchRequest{request})
		require.Equal(ht, []inboxWatchRequest{request}, changed)
	})
}

func TestInboxWatchObservedRemovalAllowsRedelivery(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		request.Oversized = inboxDrawBool(ht)
		_, seen := inboxWatchChanges(nil, []inboxWatchRequest{request})
		removed, seen := inboxWatchChanges(seen, nil)
		require.Empty(ht, removed, "removals are not wakeups")
		restored, _ := inboxWatchChanges(seen, []inboxWatchRequest{request})
		require.Equal(ht, []inboxWatchRequest{request}, restored)
	})
}

func TestInboxWatchKeepsDistinctIssueAndProjectIdentities(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		otherIssue, otherProject := request, request
		otherIssue.IssueUID += "different"
		otherProject.ProjectUID += "different"
		requests := []inboxWatchRequest{request, otherIssue, otherProject}
		changed, _ := inboxWatchChanges(nil, requests)
		require.Equal(ht, requests, changed)
	})
}

func TestInboxWatchCoalescesDuplicateIdentitiesToLastObservation(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		latest := request
		latest.value.Message += "replacement"
		changed, seen := inboxWatchChanges(nil, []inboxWatchRequest{request, latest})
		require.Equal(ht, []inboxWatchRequest{latest}, changed)
		unchanged, _ := inboxWatchChanges(seen, []inboxWatchRequest{latest})
		require.Empty(ht, unchanged)
	})
}

func TestInboxWatchIgnoresIntermediateChangesThatReturnToPriorValue(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		_, seen := inboxWatchChanges(nil, []inboxWatchRequest{request})
		intermediate := request
		intermediate.value.Message += "intermediate"
		changed, _ := inboxWatchChanges(seen, []inboxWatchRequest{intermediate, request})
		require.Empty(ht, changed)
	})
}

func TestInboxWatchOversizedHintsHaveReducedIdentity(t *testing.T) {
	checkInboxProperty(t, func(ht *inboxPropertyT) {
		request := generatedInboxWatchRequest(ht)
		_, seen := inboxWatchChanges(nil, []inboxWatchRequest{request})
		oversized := request
		oversized.Oversized = true
		oversized.value = notification.Value{}
		oversized.inboxRequest = inboxRequest{Ref: request.Ref, Project: request.Project, Title: request.Title}
		changed, seen := inboxWatchChanges(seen, []inboxWatchRequest{oversized})
		require.Equal(ht, []inboxWatchRequest{oversized}, changed)
		unchanged, seen := inboxWatchChanges(seen, []inboxWatchRequest{oversized})
		require.Empty(ht, unchanged)
		decoded, _ := inboxWatchChanges(seen, []inboxWatchRequest{request})
		require.Equal(ht, []inboxWatchRequest{request}, decoded)
	})
}

func generatedInboxWatchRequest(ht *inboxPropertyT) inboxWatchRequest {
	return inboxWatchRequest{
		Ref:        "abcd",
		Title:      "Finding",
		IssueUID:   fmt.Sprint(inboxDrawInt64(ht)),
		ProjectUID: inboxDrawText(ht),
		value: notification.Value{
			From: inboxDrawText(ht), Teammate: inboxDrawText(ht), Message: inboxDrawText(ht),
			Re: inboxDrawText(ht), Kind: inboxDrawText(ht), Broadcast: inboxDrawBool(ht),
		},
	}
}

func TestInboxWatchStopsOnReadOrOutputFailure(t *testing.T) {
	for _, failRead := range []bool{false, true} {
		t.Run(fmt.Sprint(failRead), func(t *testing.T) {
			sentinel := errors.New("unavailable")
			reads, outputs := 0, 0
			err := watchInbox(t.Context(), 0, func(context.Context) ([]inboxWatchRequest, error) {
				reads++
				if failRead {
					return nil, sentinel
				}
				return []inboxWatchRequest{{IssueUID: "issue", ProjectUID: "project"}}, nil
			}, func([]inboxWatchRequest) error {
				outputs++
				return sentinel
			})
			require.ErrorIs(t, err, sentinel)
			require.Equal(t, 1, reads)
			if failRead {
				require.Zero(t, outputs)
			} else {
				require.Equal(t, 1, outputs)
			}
		})
	}
}

func TestInboxWatchCatchesLaterAppliedStateWithoutRepeatedDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := inboxWatchRequest{IssueUID: "issue", ProjectUID: "project"}
	reads, outputs := 0, 0
	err := watchInbox(ctx, 0, func(context.Context) ([]inboxWatchRequest, error) {
		reads++
		if reads == 1 {
			return nil, nil // Before a federated change is applied.
		}
		if reads == 4 {
			cancel()
		}
		return []inboxWatchRequest{request}, nil
	}, func(got []inboxWatchRequest) error {
		outputs++
		require.Equal(t, []inboxWatchRequest{request}, got)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 4, reads)
	require.Equal(t, 1, outputs)
}

func TestInboxWatchCancellationInterruptsWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := watchInbox(ctx, time.Hour, func(context.Context) ([]inboxWatchRequest, error) {
		return []inboxWatchRequest{{IssueUID: "issue", ProjectUID: "project"}}, nil
	}, func([]inboxWatchRequest) error {
		cancel()
		return nil
	})
	require.NoError(t, err)
}

func TestInboxWatchCancellationDuringEmitIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := watchInbox(ctx, time.Hour, func(context.Context) ([]inboxWatchRequest, error) {
		return []inboxWatchRequest{{IssueUID: "issue", ProjectUID: "project"}}, nil
	}, func([]inboxWatchRequest) error {
		cancel()
		return ctx.Err()
	})
	require.NoError(t, err)
}
