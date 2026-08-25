package rootbridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/pkg/connector"
)

func TestRegistryConstructionDoesNotProbeBlockingConnector(t *testing.T) {
	client := &blockingDescribeClient{entered: make(chan struct{}), release: make(chan struct{})}
	type result struct {
		registry *Registry
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
			ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
		}}, func(config.ConnectorConfig) connectorclient.Client { return client })
		finished <- result{registry: registry, err: err}
	}()

	select {
	case <-client.entered:
		close(client.release)
		<-finished
		t.Fatal("registry construction called Describe")
	case got := <-finished:
		require.NoError(t, got.err)
		require.NotNil(t, got.registry)
		assert.Zero(t, client.calls.Load())
	}
}

func TestRegistryExplicitRefreshRetainsOnlySafeHealth(t *testing.T) {
	client := &fakeConnectorClient{describeErr: errors.New("request failed: token=private-value")}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)

	initial, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.Empty(t, initial.HealthError)
	_, err = registry.Refresh(t.Context(), "notes")
	require.Error(t, err)
	snapshot, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.Empty(t, snapshot.ConnectorID)
	assert.Equal(t, "external connector request failed", snapshot.HealthError)
	assert.NotContains(t, snapshot.HealthError, "token=")
	assert.NotContains(t, snapshot.HealthError, "private-value")
}

func TestRegistryRetainsSafeConnectorFailureDiagnostics(t *testing.T) {
	const rawDiagnostic = "private executable path and credential detail"
	for _, tc := range []struct {
		name    string
		failure error
		kind    error
		want    string
	}{
		{
			name: "process", failure: errors.Join(connectorclient.ErrProcessFailure, errors.New(rawDiagnostic)),
			kind: connectorclient.ErrProcessFailure, want: "external connector process failed",
		},
		{
			name: "protocol", failure: errors.Join(connectorclient.ErrProtocolFailure, errors.New(rawDiagnostic)),
			kind: connectorclient.ErrProtocolFailure, want: "external connector protocol failed",
		},
		{
			name:    "child timeout",
			failure: errors.Join(connectorclient.ErrRequestTimeout, context.DeadlineExceeded, errors.New(rawDiagnostic)),
			kind:    connectorclient.ErrRequestTimeout, want: "external connector request timed out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeConnectorClient{describeErr: tc.failure}
			registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
				ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
			}}, func(config.ConnectorConfig) connectorclient.Client { return client })
			require.NoError(t, err)

			_, err = registry.Refresh(t.Context(), "notes")

			require.ErrorIs(t, err, ErrConnectorCall)
			require.ErrorIs(t, err, tc.kind)
			assert.EqualError(t, err, tc.want)
			assert.NotContains(t, err.Error(), rawDiagnostic)
			snapshot, ok := registry.Snapshot("notes")
			require.True(t, ok)
			assert.Equal(t, tc.want, snapshot.HealthError)
			assert.NotContains(t, snapshot.HealthError, rawDiagnostic)
		})
	}
}

func TestRegistryExplicitConnectorCategoryWinsParentEndBeforeBoundary(t *testing.T) {
	const rawDiagnostic = "opaque process path and stderr detail"
	for _, tc := range []struct {
		name     string
		category error
		want     string
	}{
		{
			name: "process", category: connectorclient.ErrProcessFailure,
			want: "external connector process failed",
		},
		{
			name: "protocol", category: connectorclient.ErrProtocolFailure,
			want: "external connector protocol failed",
		},
		{
			name: "child timeout", category: connectorclient.ErrRequestTimeout,
			want: "external connector request timed out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			client := &fakeConnectorClient{
				description:          testDescription("account-1"),
				describeErr:          errors.Join(tc.category, errors.New(rawDiagnostic)),
				beforeDescribeReturn: cancel,
			}
			registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
				ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
			}}, func(config.ConnectorConfig) connectorclient.Client { return client })
			require.NoError(t, err)

			_, err = registry.Refresh(ctx, "notes")

			require.ErrorIs(t, err, ErrConnectorCall)
			require.ErrorIs(t, err, tc.category)
			assert.EqualError(t, err, tc.want)
			assert.NotContains(t, err.Error(), rawDiagnostic)
			snapshot, ok := registry.Snapshot("notes")
			require.True(t, ok)
			assert.Equal(t, tc.want, snapshot.HealthError)
		})
	}
}

func TestRegistryRefreshReplacesHealthWithDescription(t *testing.T) {
	client := &fakeConnectorClient{describeErr: errors.New("connector unavailable")}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "notes")
	require.Error(t, err)

	client.describeErr = nil
	client.description = testDescription("account-1")
	got, err := registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)
	assert.Equal(t, "example.connector", got.ConnectorID)
	assert.Empty(t, got.HealthError)
}

func TestRegistryFieldsSuccessClearsStaleHealthWithoutChangingIdentity(t *testing.T) {
	description := testDescription("account-1")
	client := &fakeConnectorClient{description: description}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	want, err := registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)

	client.listFieldsErr = errors.New("synthetic connector failure")
	_, err = registry.Fields(t.Context(), "notes")
	require.Error(t, err)
	unhealthy, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.NotEmpty(t, unhealthy.HealthError)

	client.listFieldsErr = nil
	client.fields = []connector.FieldDescriptor{{
		ID: "schedule", DisplayName: "Schedule", AcceptedKinds: []string{"date"},
		Nullable: true, Writable: true, SchemaRevision: "schema-1",
	}}
	fields, err := registry.Fields(t.Context(), "notes")
	require.NoError(t, err)
	require.Len(t, fields, 1)
	healthy, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.Empty(t, healthy.HealthError)
	want.HealthError = ""
	assert.Equal(t, want, healthy)
}

func TestRegistryFieldsRequiresAcceptedCapabilityBeforeDiscovery(t *testing.T) {
	description := testDescription("account-1")
	description.Capabilities = nil
	client := &fakeConnectorClient{description: description}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)

	_, err = registry.Fields(t.Context(), "notes")

	assert.ErrorIs(t, err, ErrFieldSynchronizationUnavailable)
	assert.Zero(t, client.listFieldsCalls)
}

func TestRegistryFieldsSuccessDoesNotClearIdentityDriftHealth(t *testing.T) {
	client := &fakeConnectorClient{description: testDescription("account-1")}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)

	client.description.ConnectorID = "changed.connector"
	identitySnapshot, err := registry.Refresh(t.Context(), "notes")
	require.ErrorIs(t, err, ErrConnectorIdentityChanged)
	require.NotEmpty(t, identitySnapshot.HealthError)
	client.fields = []connector.FieldDescriptor{{
		ID: "schedule", DisplayName: "Schedule", AcceptedKinds: []string{"date"},
		Nullable: true, Writable: true, SchemaRevision: "schema-1",
	}}
	_, err = registry.Fields(t.Context(), "notes")
	require.NoError(t, err)

	got, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.Equal(t, identitySnapshot.HealthError, got.HealthError)
	client.description = testDescription("account-1")
	recovered, err := registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)
	assert.Empty(t, recovered.HealthError)
}

func TestRegistryFieldsFailureDoesNotReplaceIdentityHealth(t *testing.T) {
	client := &fakeConnectorClient{description: testDescription("account-1")}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)

	client.description.ConnectorID = "changed.connector"
	identitySnapshot, err := registry.Refresh(t.Context(), "notes")
	require.ErrorIs(t, err, ErrConnectorIdentityChanged)
	client.listFieldsErr = errors.New("synthetic field probe failure")
	_, err = registry.Fields(t.Context(), "notes")
	require.Error(t, err)

	got, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.Equal(t, identitySnapshot.HealthError, got.HealthError)
	assert.NotContains(t, got.HealthError, "field probe")
}

func TestRegistryMissingHealthWriteDoesNotCreatePhantomInstance(t *testing.T) {
	registry, err := NewRegistry(t.Context(), nil, nil)
	require.NoError(t, err)

	registry.recordDescribeHealthError("missing", errors.New("synthetic probe failure"))
	registry.recordFieldHealthError("missing", errors.New("synthetic field failure"))
	registry.clearFieldHealthError("missing")

	assert.Empty(t, registry.Snapshots())
	_, ok := registry.Snapshot("missing")
	assert.False(t, ok)
}

func TestRegistryWrapsEveryConnectorClientBoundary(t *testing.T) {
	const rawDiagnostic = "opaque child-process detail"
	client := &allMethodsErrorConnectorClient{err: errors.New(rawDiagnostic)}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	instance, ok := registry.instance("notes")
	require.True(t, ok)
	calls := []struct {
		name string
		call func(context.Context, connectorclient.Client) error
	}{
		{name: "describe", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.Describe(ctx)
			return err
		}},
		{name: "resolve root", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.ResolveRoot(ctx, connector.ResolveRootParams{})
			return err
		}},
		{name: "read root", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.ReadRoot(ctx, connector.ReadRootParams{})
			return err
		}},
		{name: "list comments", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.ListComments(ctx, connector.ListCommentsParams{})
			return err
		}},
		{name: "complete root", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.CompleteRoot(ctx, connector.CompleteRootParams{})
			return err
		}},
		{name: "publish comment", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.PublishComment(ctx, connector.PublishCommentParams{})
			return err
		}},
		{name: "list fields", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.ListFields(ctx)
			return err
		}},
		{name: "read fields", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.ReadFields(ctx, connector.ReadFieldsParams{})
			return err
		}},
		{name: "write fields", call: func(ctx context.Context, client connectorclient.Client) error {
			_, err := client.WriteFields(ctx, connector.WriteFieldsParams{})
			return err
		}},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			err := call.call(t.Context(), instance.Client)
			require.ErrorIs(t, err, ErrConnectorCall)
			assert.NotContains(t, err.Error(), rawDiagnostic)
		})
	}
}

func TestRegistryConnectorBoundaryRejectsNonCanonicalFieldDescriptors(t *testing.T) {
	for _, test := range []struct {
		name       string
		descriptor connector.FieldDescriptor
	}{
		{
			name: "padded field ID",
			descriptor: connector.FieldDescriptor{
				ID: " field-1 ", DisplayName: "Start", AcceptedKinds: []string{"date"},
				Nullable: true, Writable: true, SchemaRevision: "schema-1",
			},
		},
		{
			name: "padded schema revision",
			descriptor: connector.FieldDescriptor{
				ID: "field-1", DisplayName: "Start", AcceptedKinds: []string{"date"},
				Nullable: true, Writable: true, SchemaRevision: " schema-1 ",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeConnectorClient{
				description: testDescription("account-1"), fields: []connector.FieldDescriptor{test.descriptor},
			}
			registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
				ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
			}}, func(config.ConnectorConfig) connectorclient.Client { return client })
			require.NoError(t, err)
			instance, ok := registry.instance("notes")
			require.True(t, ok)

			_, err = instance.Client.ListFields(t.Context())
			require.ErrorIs(t, err, connectorclient.ErrProtocolFailure)
			assert.EqualError(t, err, "external connector protocol failed")
		})
	}
}

func TestRegistryConnectorBoundaryPreservesTypedAndContextErrors(t *testing.T) {
	client := &allMethodsErrorConnectorClient{}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	instance, ok := registry.instance("notes")
	require.True(t, ok)

	typed := &connector.Error{Code: "remote_failed", Message: "safe external failure"}
	client.err = typed
	_, err = instance.Client.Describe(t.Context())
	assert.Same(t, typed, err)
	assert.NotErrorIs(t, err, ErrConnectorCall)

	client.err = errors.New("opaque cancellation detail")
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = instance.Client.Describe(canceledCtx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrConnectorCall)
	deadlineCtx, deadlineCancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer deadlineCancel()
	_, err = instance.Client.Describe(deadlineCtx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrConnectorCall)

	client.err = context.DeadlineExceeded
	_, err = instance.Client.Describe(t.Context())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorIs(t, err, ErrConnectorCall)
}

func TestRegistryParentContextEndDoesNotMutateProbeHealth(t *testing.T) {
	contextCases := []struct {
		name string
		new  func(*testing.T) (context.Context, error)
	}{
		{
			name: "canceled",
			new: func(t *testing.T) (context.Context, error) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx, context.Canceled
			},
		},
		{
			name: "deadline",
			new: func(t *testing.T) (context.Context, error) {
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx, context.DeadlineExceeded
			},
		},
	}
	probeCases := []struct {
		name   string
		setErr func(*fakeConnectorClient, error)
		call   func(context.Context, *Registry) error
	}{
		{
			name:   "describe",
			setErr: func(client *fakeConnectorClient, err error) { client.describeErr = err },
			call: func(ctx context.Context, registry *Registry) error {
				_, err := registry.Refresh(ctx, "notes")
				return err
			},
		},
		{
			name:   "fields",
			setErr: func(client *fakeConnectorClient, err error) { client.listFieldsErr = err },
			call: func(ctx context.Context, registry *Registry) error {
				_, err := registry.Fields(ctx, "notes")
				return err
			},
		},
	}
	for _, contextCase := range contextCases {
		for _, probeCase := range probeCases {
			for _, priorHealth := range []bool{false, true} {
				name := contextCase.name + "/" + probeCase.name
				if priorHealth {
					name += "/prior-health"
				} else {
					name += "/fresh"
				}
				t.Run(name, func(t *testing.T) {
					client := &fakeConnectorClient{description: testDescription("account-1")}
					registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
						ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
					}}, func(config.ConnectorConfig) connectorclient.Client { return client })
					require.NoError(t, err)
					wantHealth := ""
					if priorHealth {
						probeCase.setErr(client, errors.New("synthetic prior probe failure"))
						require.Error(t, probeCase.call(t.Context(), registry))
						snapshot, ok := registry.Snapshot("notes")
						require.True(t, ok)
						require.NotEmpty(t, snapshot.HealthError)
						wantHealth = snapshot.HealthError
					}

					probeCase.setErr(client, errors.New("opaque canceled probe detail"))
					ctx, wantErr := contextCase.new(t)
					err = probeCase.call(ctx, registry)
					require.ErrorIs(t, err, wantErr)
					snapshot, ok := registry.Snapshot("notes")
					require.True(t, ok)
					assert.Equal(t, wantHealth, snapshot.HealthError)
				})
			}
		}
	}
}

func TestRegistryLiveParentProbeFailuresRecordHealth(t *testing.T) {
	probeCases := []struct {
		name   string
		setErr func(*fakeConnectorClient, error)
		call   func(context.Context, *Registry) error
	}{
		{
			name:   "describe",
			setErr: func(client *fakeConnectorClient, err error) { client.describeErr = err },
			call: func(ctx context.Context, registry *Registry) error {
				_, err := registry.Refresh(ctx, "notes")
				return err
			},
		},
		{
			name:   "fields",
			setErr: func(client *fakeConnectorClient, err error) { client.listFieldsErr = err },
			call: func(ctx context.Context, registry *Registry) error {
				_, err := registry.Fields(ctx, "notes")
				return err
			},
		},
	}
	errorCases := []struct {
		name      string
		err       error
		wantError error
	}{
		{name: "child deadline", err: context.DeadlineExceeded, wantError: ErrConnectorCall},
		{name: "typed connector error", err: &connector.Error{Code: "remote_failed", Message: "safe external failure"}},
	}
	for _, probeCase := range probeCases {
		for _, errorCase := range errorCases {
			t.Run(probeCase.name+"/"+errorCase.name, func(t *testing.T) {
				client := &fakeConnectorClient{description: testDescription("account-1")}
				probeCase.setErr(client, errorCase.err)
				registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
					ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
				}}, func(config.ConnectorConfig) connectorclient.Client { return client })
				require.NoError(t, err)

				err = probeCase.call(t.Context(), registry)
				require.Error(t, err)
				if errorCase.wantError != nil {
					assert.ErrorIs(t, err, errorCase.wantError)
				} else {
					assert.Same(t, errorCase.err, err)
				}
				snapshot, ok := registry.Snapshot("notes")
				require.True(t, ok)
				assert.NotEmpty(t, snapshot.HealthError)
			})
		}
	}
}

func TestRegistryConnectorFailureRemainsHealthWhenParentExpiresAfterBoundary(t *testing.T) {
	probeCases := []struct {
		name   string
		setErr func(*fakeConnectorClient, error)
		hook   func(*boundaryReturnHookClient, func())
		call   func(context.Context, *Registry) error
	}{
		{
			name:   "describe",
			setErr: func(client *fakeConnectorClient, err error) { client.describeErr = err },
			hook: func(client *boundaryReturnHookClient, after func()) {
				client.afterDescribe = after
			},
			call: func(ctx context.Context, registry *Registry) error {
				_, err := registry.Refresh(ctx, "notes")
				return err
			},
		},
		{
			name:   "fields",
			setErr: func(client *fakeConnectorClient, err error) { client.listFieldsErr = err },
			hook: func(client *boundaryReturnHookClient, after func()) {
				client.afterListFields = after
			},
			call: func(ctx context.Context, registry *Registry) error {
				_, err := registry.Fields(ctx, "notes")
				return err
			},
		},
	}
	typedFailure := &connector.Error{Code: "remote_failed", Message: "safe external failure"}
	errorCases := []struct {
		name        string
		err         error
		assertError func(*testing.T, error)
		wantHealth  string
	}{
		{
			name: "child deadline", err: errors.Join(connectorclient.ErrRequestTimeout, context.DeadlineExceeded),
			assertError: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrConnectorCall)
				require.ErrorIs(t, err, connectorclient.ErrRequestTimeout)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			},
			wantHealth: "external connector request timed out",
		},
		{
			name: "typed connector error", err: errors.Join(typedFailure, context.DeadlineExceeded),
			assertError: func(t *testing.T, err error) {
				var gotTyped *connector.Error
				require.ErrorAs(t, err, &gotTyped)
				assert.Same(t, typedFailure, gotTyped)
				assert.ErrorIs(t, err, context.DeadlineExceeded)
				assert.NotErrorIs(t, err, ErrConnectorCall)
			},
		},
	}
	for _, probeCase := range probeCases {
		for _, errorCase := range errorCases {
			t.Run(probeCase.name+"/"+errorCase.name, func(t *testing.T) {
				rawClient := &fakeConnectorClient{description: testDescription("account-1")}
				probeCase.setErr(rawClient, errorCase.err)
				registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
					ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
				}}, func(config.ConnectorConfig) connectorclient.Client { return rawClient })
				require.NoError(t, err)
				ctx := newControlledEndContext(t.Context())
				registry.mu.Lock()
				instance := registry.instances["notes"]
				hookedClient := &boundaryReturnHookClient{Client: instance.Client}
				probeCase.hook(hookedClient, func() { ctx.end(context.DeadlineExceeded) })
				instance.Client = hookedClient
				registry.instances["notes"] = instance
				registry.mu.Unlock()

				err = probeCase.call(ctx, registry)

				errorCase.assertError(t, err)
				snapshot, ok := registry.Snapshot("notes")
				require.True(t, ok)
				if errorCase.wantHealth != "" {
					assert.Equal(t, errorCase.wantHealth, snapshot.HealthError)
				} else {
					assert.NotEmpty(t, snapshot.HealthError)
				}
			})
		}
	}
}

func TestRegistryRefreshRejectsChangedStableConnectorIdentity(t *testing.T) {
	client := &fakeConnectorClient{description: testDescription("account-1")}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)

	client.description.ConnectorID = "changed.connector"
	got, err := registry.Refresh(t.Context(), "notes")
	assert.ErrorIs(t, err, ErrConnectorIdentityChanged)
	assert.Equal(t, "example.connector", got.ConnectorID)
	assert.NotEmpty(t, got.HealthError)
}

func TestRegistryConcurrentFirstRefreshPinsExactlyOneConnectorIdentity(t *testing.T) {
	client := &concurrentDescribeClient{
		entered: make(chan struct{}, 2), release: make(chan struct{}),
		descriptions: [2]connector.Description{
			testDescription("account-1"), testDescription("account-1"),
		},
	}
	client.descriptions[1].ConnectorID = "alternate.connector"
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := registry.Refresh(context.Background(), "notes")
			results <- err
		}()
	}
	<-client.entered
	<-client.entered
	close(client.release)
	var accepted, rejected int
	for range 2 {
		err := <-results
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrConnectorIdentityChanged) {
			rejected++
		} else {
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 1, rejected)
	snapshot, ok := registry.Snapshot("notes")
	require.True(t, ok)
	assert.Contains(t, []string{"example.connector", "alternate.connector"}, snapshot.ConnectorID)
	assert.NotEmpty(t, snapshot.HealthError)
}

func TestRegistrySnapshotOmitsPrivateDescriptionAndValidationValues(t *testing.T) {
	const (
		accountIdentity = "account-reference-42"
		opaqueValue     = "opaque-7f3c2a"
	)
	description := testDescription(accountIdentity)
	description.SelfActorID = opaqueValue + "-actor"
	description.ConfigSchema = json.RawMessage(`{"default":"` + opaqueValue + `-schema"}`)
	client := &fakeConnectorClient{description: description}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	snapshot, err := registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)
	assert.Equal(t, "example.connector", snapshot.ConnectorID)
	assert.Equal(t, "Example Connector", snapshot.DisplayName)
	assert.Equal(t, connector.ProtocolVersion, snapshot.Protocol)
	assert.Equal(t, accountIdentity, snapshot.AccountIdentity)
	encoded := assertJSON(t, snapshot)
	assert.Contains(t, encoded, accountIdentity)
	assert.NotContains(t, encoded, opaqueValue)

	invalid := testDescription("account-1")
	invalid.Protocol = opaqueValue + "-protocol"
	invalidRegistry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "invalid", Command: filepath.Join(t.TempDir(), "connector"),
	}}, func(config.ConnectorConfig) connectorclient.Client {
		return &fakeConnectorClient{description: invalid}
	})
	require.NoError(t, err)
	snapshot, err = invalidRegistry.Refresh(t.Context(), "invalid")
	require.Error(t, err)
	assert.NotContains(t, assertJSON(t, snapshot), opaqueValue)
	assert.Equal(t, "connector description is invalid", snapshot.HealthError)
}

func TestRegistrySnapshotsExcludeProcessConfiguration(t *testing.T) {
	client := &fakeConnectorClient{description: testDescription("account-1")}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: filepath.Join(t.TempDir(), "connector"),
		Args: []string{"--private"}, Env: map[string]string{"TOKEN": "PRIVATE_SOURCE"},
		Settings: map[string]any{"credential": "private-value"},
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "notes")
	require.NoError(t, err)

	snapshots := registry.Snapshots()
	require.Len(t, snapshots, 1)
	encoded := assertJSON(t, snapshots)
	assert.NotContains(t, encoded, "--private")
	assert.NotContains(t, encoded, "PRIVATE_SOURCE")
	assert.NotContains(t, encoded, "private-value")
}

func TestRegistryRefreshSnapshotsIsOrderedAndKeepsFailuresLocal(t *testing.T) {
	clients := map[string]*fakeConnectorClient{
		"alpha": {description: testDescription("account-alpha")},
		"zeta": {
			describeErr: errors.Join(
				connectorclient.ErrProcessFailure,
				errors.New("private connector diagnostic"),
			),
		},
	}
	registry, err := NewRegistry(t.Context(), []config.ConnectorConfig{
		{ID: "zeta", Command: filepath.Join(t.TempDir(), "zeta-connector")},
		{ID: "alpha", Command: filepath.Join(t.TempDir(), "alpha-connector")},
	}, func(cfg config.ConnectorConfig) connectorclient.Client { return clients[cfg.ID] })
	require.NoError(t, err)

	snapshots := registry.RefreshSnapshots(t.Context())

	require.Len(t, snapshots, 2)
	assert.Equal(t, "alpha", snapshots[0].ID)
	assert.Equal(t, "example.connector", snapshots[0].ConnectorID)
	assert.Empty(t, snapshots[0].HealthError)
	assert.Equal(t, "zeta", snapshots[1].ID)
	assert.Empty(t, snapshots[1].ConnectorID)
	assert.Equal(t, "external connector process failed", snapshots[1].HealthError)
	assert.NotContains(t, snapshots[1].HealthError, "private")
}

func TestRegistryRejectsInvalidConfigurationBeforeConstructingClients(t *testing.T) {
	constructed := 0
	_, err := NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "notes", Command: "relative-connector",
	}}, func(config.ConnectorConfig) connectorclient.Client {
		constructed++
		return &fakeConnectorClient{}
	})
	require.Error(t, err)
	assert.Zero(t, constructed)
}

func testDescription(account string) connector.Description {
	return connector.Description{
		ConnectorID: "example.connector", DisplayName: "Example Connector",
		Protocol: connector.ProtocolVersion, AccountIdentity: account,
		Capabilities: []connector.Capability{connector.CapabilityFields, connector.CapabilityConditionalFields},
	}
}

func assertJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}

type blockingDescribeClient struct {
	fakeConnectorClient
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (c *blockingDescribeClient) Describe(context.Context) (connector.Description, error) {
	c.calls.Add(1)
	close(c.entered)
	<-c.release
	return connector.Description{}, errors.New("connector unavailable")
}

type concurrentDescribeClient struct {
	fakeConnectorClient
	entered      chan struct{}
	release      chan struct{}
	next         atomic.Int64
	descriptions [2]connector.Description
}

type allMethodsErrorConnectorClient struct {
	err error
}

type controlledEndContext struct {
	context.Context
	done chan struct{}
	mu   sync.RWMutex
	err  error
}

func newControlledEndContext(parent context.Context) *controlledEndContext {
	return &controlledEndContext{Context: parent, done: make(chan struct{})}
}

func (c *controlledEndContext) Done() <-chan struct{} { return c.done }

func (c *controlledEndContext) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.err
}

func (c *controlledEndContext) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	close(c.done)
}

type boundaryReturnHookClient struct {
	connectorclient.Client
	afterDescribe   func()
	afterListFields func()
}

func (c *boundaryReturnHookClient) Describe(ctx context.Context) (connector.Description, error) {
	result, err := c.Client.Describe(ctx)
	if c.afterDescribe != nil {
		c.afterDescribe()
	}
	return result, err
}

func (c *boundaryReturnHookClient) ListFields(ctx context.Context) (connector.ListFieldsResult, error) {
	result, err := c.Client.ListFields(ctx)
	if c.afterListFields != nil {
		c.afterListFields()
	}
	return result, err
}

func (c *allMethodsErrorConnectorClient) Describe(context.Context) (connector.Description, error) {
	return connector.Description{}, c.err
}

func (c *allMethodsErrorConnectorClient) ResolveRoot(context.Context, connector.ResolveRootParams) (connector.Root, error) {
	return connector.Root{}, c.err
}

func (c *allMethodsErrorConnectorClient) ReadRoot(context.Context, connector.ReadRootParams) (connector.Root, error) {
	return connector.Root{}, c.err
}

func (c *allMethodsErrorConnectorClient) ListComments(context.Context, connector.ListCommentsParams) (connector.ListCommentsResult, error) {
	return connector.ListCommentsResult{}, c.err
}

func (c *allMethodsErrorConnectorClient) CompleteRoot(context.Context, connector.CompleteRootParams) (connector.Root, error) {
	return connector.Root{}, c.err
}

func (c *allMethodsErrorConnectorClient) PublishComment(context.Context, connector.PublishCommentParams) (connector.Comment, error) {
	return connector.Comment{}, c.err
}

func (c *allMethodsErrorConnectorClient) ListFields(context.Context) (connector.ListFieldsResult, error) {
	return connector.ListFieldsResult{}, c.err
}

func (c *allMethodsErrorConnectorClient) ReadFields(context.Context, connector.ReadFieldsParams) (connector.ReadFieldsResult, error) {
	return connector.ReadFieldsResult{}, c.err
}

func (c *allMethodsErrorConnectorClient) WriteFields(context.Context, connector.WriteFieldsParams) (connector.WriteFieldsResult, error) {
	return connector.WriteFieldsResult{}, c.err
}

func (c *concurrentDescribeClient) Describe(context.Context) (connector.Description, error) {
	index := c.next.Add(1) - 1
	c.entered <- struct{}{}
	<-c.release
	return c.descriptions[index], nil
}
