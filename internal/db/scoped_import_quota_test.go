package db

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests isolate accounting on already-staged typed indexes. Opaque byte
// fields deliberately are not hashed, signed, or decoded as complete objects;
// full-graph/crypto/restore positives live in the owner-backup fixtures. Distinct
// row identities reuse a single immutable 1 MiB buffer: logical persisted bytes
// accumulate without allocating a daemon-sized backing array.
func quotaUID(n int) string { return fmt.Sprintf("%026d", n) }

func quotaSizedRecords(t *testing.T, total, overhead int64, backing []byte, row func(int, []byte) ImportRecord) []ImportRecord {
	t.Helper()
	count := (total + overhead + int64(len(backing)) - 1) / (overhead + int64(len(backing)))
	remaining := total - count*overhead
	require.GreaterOrEqual(t, remaining, int64(0))
	records := make([]ImportRecord, 0, int(count))
	for i := 0; i < int(count); i++ {
		n := min(remaining, int64(len(backing)))
		record := row(i, backing[:int(n)])
		require.Equal(t, overhead+n, ScopedRetainedBytes(record), "independent field-size contract for row %d", i)
		records = append(records, record)
		remaining -= n
	}
	require.Zero(t, remaining)
	return records
}

func TestScopedImportQuotaActiveViews(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("cap%+d", delta), func(t *testing.T) {
			g := scopedImportGraph{views: map[string]*ScopedViewExport{}}
			for i := 0; i < MaxScopedActiveViews+delta; i++ {
				state := "active"
				if i%2 == 1 {
					state = "prepared"
				}
				id := quotaUID(i + 1)
				g.views[id] = &ScopedViewExport{ViewUID: id, AuthorityUID: quotaUID(9001), BindingUID: quotaUID(9002), State: state}
			}
			err := g.validateQuotas(nil)
			if delta > 0 {
				require.ErrorContains(t, err, "scoped active view quota exceeded")
			} else {
				require.NoError(t, err)
			}
		})
	}
	t.Run("independent_authority_binding_and_retired", func(t *testing.T) {
		g := scopedImportGraph{views: map[string]*ScopedViewExport{}}
		scopes := [][2]string{{quotaUID(9001), quotaUID(9002)}, {quotaUID(9003), quotaUID(9002)}, {quotaUID(9001), quotaUID(9004)}}
		n := 0
		for _, scope := range scopes {
			for i := 0; i < MaxScopedActiveViews; i++ {
				n++
				id := quotaUID(n)
				g.views[id] = &ScopedViewExport{ViewUID: id, AuthorityUID: scope[0], BindingUID: scope[1], State: "active"}
			}
		}
		for _, state := range []string{"retired", "fenced"} {
			for i := 0; i < MaxScopedActiveViews+1; i++ {
				n++
				id := quotaUID(n)
				g.views[id] = &ScopedViewExport{ViewUID: id, AuthorityUID: scopes[0][0], BindingUID: scopes[0][1], State: state}
			}
		}
		require.NoError(t, g.validateQuotas(nil), "active/prepared cap is per authority+binding; retired/fenced obligations stay retained")
	})
}

func TestScopedImportQuotaBindingBytes(t *testing.T) {
	backing := make([]byte, 1<<20)
	binding := quotaUID(9002)
	// Binding UID26 + epoch8 + direction8 + channel4 + sequence8 + object UID26
	// + object/offer digests128 + exact header32 + three status booleans3.
	const overhead int64 = 26 + 8 + 8 + 4 + 8 + 26 + 128 + 32 + 3
	makeRow := func(i int, payload []byte) ImportRecord {
		return &ScopedDeliveryRecord{ScopedDeliveryRef: ScopedDeliveryRef{ScopedDeliveryStream: ScopedDeliveryStream{BindingUID: binding, Epoch: 1, Direction: ScopedDeliveryOutbound, Channel: ScopedDeliveryView}, Sequence: int64(i + 1)}, ObjectUID: quotaUID(i + 100), ObjectDigest: quotaDigest, OfferedDigest: quotaDigest, HeaderBytes: backing[:32], EnvelopeBytes: payload}
	}
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprintf("cap%+d", delta), func(t *testing.T) {
			// Include one real nil-envelope tombstone; its immutable identity/header
			// still consumes the same independent fixed-byte budget.
			records := quotaSizedRecords(t, MaxScopedBindingBytes+delta-overhead, overhead, backing, makeRow)
			tombstone := makeRow(1<<20, nil)
			require.Equal(t, overhead, ScopedRetainedBytes(tombstone))
			records = append(records, tombstone)
			err := (&scopedImportGraph{}).validateQuotas(records)
			if delta > 0 {
				require.ErrorContains(t, err, "scoped binding retained-byte quota exceeded")
			} else {
				require.NoError(t, err)
			}
		})
	}
	t.Run("separate_bindings", func(t *testing.T) {
		first := quotaSizedRecords(t, MaxScopedBindingBytes, overhead, backing, makeRow)
		second := quotaSizedRecords(t, MaxScopedBindingBytes, overhead, backing, func(i int, b []byte) ImportRecord {
			r := makeRow(i, b).(*ScopedDeliveryRecord)
			r.BindingUID = quotaUID(9003)
			return r
		})
		require.NoError(t, (&scopedImportGraph{}).validateQuotas(append(first, second...)), "each binding independently fits despite larger daemon total")
	})
}

const quotaDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func quotaPrincipal() CustodyPrincipal {
	return CustodyPrincipal{AuthorityUID: quotaUID(9001), BindingUID: quotaUID(9002), Epoch: 1, AccountableActor: "member", RouteDigest: quotaDigest}
}
func quotaPrivateViews() map[string]*ScopedViewExport {
	p := quotaPrincipal()
	views := map[string]*ScopedViewExport{}
	for i := 1; i <= 4; i++ {
		id := quotaUID(i)
		views[id] = &ScopedViewExport{ViewUID: id, AuthorityUID: p.AuthorityUID, BindingUID: p.BindingUID, RouteDigest: p.RouteDigest, State: "retired"}
	}
	views[quotaUID(3)].BindingUID = quotaUID(9003)
	views[quotaUID(4)].RouteDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	return views
}
func quotaWork(t *testing.T, view string, total int64, backing []byte) *ScopedWorkExport {
	t.Helper()
	// View UID26 + dirty revision8 + captured state8; optional capture identity
	// fields are absent in this isolated size test (not a full work-shape proof).
	const overhead int64 = 26 + 8 + 8
	require.GreaterOrEqual(t, total, overhead)
	require.LessOrEqual(t, total-overhead, int64(len(backing)))
	row := &ScopedWorkExport{ViewUID: view, State: "captured", CaptureBytes: backing[:int(total-overhead)]}
	require.Equal(t, total, ScopedRetainedBytes(row))
	return row
}

func TestScopedImportQuotaPrivateRecovery(t *testing.T) {
	backing := make([]byte, 1<<20)
	p := quotaPrincipal()
	principal, err := json.Marshal(p)
	require.NoError(t, err)
	for _, family := range []string{"source_manifest", "private_original", "control_return"} {
		t.Run(family, func(t *testing.T) {
			for _, delta := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("cap%+d", delta), func(t *testing.T) {
					g := scopedImportGraph{views: quotaPrivateViews(), submissions: map[string]SourceSubmission{}}
					var overhead int64
					var makeRow func(int, []byte) ImportRecord
					switch family {
					case "source_manifest":
						// UID26 + digest64 + principal bytes + principal key64 + staged6 +
						// pending7 + cancel boolean1. Only ManifestBytes varies.
						overhead = 26 + 64 + int64(len(principal)) + 64 + 6 + 7 + 1
						makeRow = func(i int, b []byte) ImportRecord {
							id := quotaUID(i + 100)
							g.submissions[id] = SourceSubmission{Operation: SourceOperation{UID: id, Principal: p}}
							return &ScopedSourceOperationExport{OperationUID: id, Digest: quotaDigest, ManifestBytes: b, PrincipalBytes: principal, PrincipalKey: quotaDigest, State: "staged", OverlayState: "pending"}
						}
					case "private_original":
						// Event UID26 + content hash64 + project UID26 + operation UID26 + ordinal8.
						overhead = 26 + 64 + 26 + 26 + 8
						makeRow = func(i int, b []byte) ImportRecord {
							id := quotaUID(i + 100)
							g.submissions[id] = SourceSubmission{Operation: SourceOperation{UID: id, Principal: p}}
							ordinal := int64(0)
							return &ScopedPrivateSourceExport{EventUID: quotaUID(i + 1000), ContentHash: quotaDigest, ProjectUID: quotaUID(9005), OperationUID: &id, Ordinal: &ordinal, EventBytes: b}
						}
					case "control_return":
						// Request UID26 + binding UID26 + request digest64 + principal bytes +
						// pending state7 + result-ACK boolean1. Only ResultBytes varies.
						overhead = 26 + 26 + 64 + int64(len(principal)) + 7 + 1
						makeRow = func(i int, b []byte) ImportRecord {
							return &ScopedControlCustodyExport{RequestUID: quotaUID(i + 100), BindingUID: p.BindingUID, RequestDigest: quotaDigest, PrincipalBytes: principal, ResultBytes: b, State: "pending"}
						}
					}
					const workSize int64 = 1 << 20
					records := quotaSizedRecords(t, MaxScopedRecoveryBytes-workSize+delta, overhead, backing, makeRow)
					records = append(records, quotaWork(t, quotaUID(1), workSize, backing), quotaWork(t, quotaUID(3), workSize, backing), quotaWork(t, quotaUID(4), workSize, backing))
					err := g.validateQuotas(records)
					if delta > 0 {
						require.ErrorContains(t, err, "scoped private recovery quota exceeded")
						g.views[quotaUID(1)].RouteDigest = g.views[quotaUID(4)].RouteDigest
						require.NoError(t, g.validateQuotas(records), "other matching view receives shared bytes once; unmatched route/binding contributes nothing")
					} else {
						require.NoError(t, err, "shared binding/route is charged once independently per view, plus only its own work row")
					}
				})
			}
		})
	}
	t.Run("work_is_view_local", func(t *testing.T) {
		g := scopedImportGraph{views: quotaPrivateViews()}
		// Each distinct view has exactly one work row, respecting the work PK.
		// Their combined bytes exceed one private budget, but a shared route
		// cannot combine their independent per-view recovery budgets.
		records := []ImportRecord{}
		for i := 1; i <= MaxScopedActiveViews; i++ {
			id := quotaUID(i)
			g.views[id] = &ScopedViewExport{ViewUID: id, BindingUID: p.BindingUID, RouteDigest: p.RouteDigest, State: "retired"}
			records = append(records, quotaWork(t, id, 1<<20, backing))
		}
		require.NoError(t, g.validateQuotas(records), "work bytes count only toward their owning view, never every view sharing the route")
	})
}

func TestScopedImportQuotaAggregateBytes(t *testing.T) {
	backing := make([]byte, 1<<20)
	nativeValue := string(backing)
	// View UID26 + generation8 + sequence8 + digest64 + previous digest64 +
	// capture UID26. Unique batch sequence/digest indexes share immutable bytes.
	// Digests here are distinct size-only placeholders, never claimed crypto.
	const overhead int64 = 26 + 8 + 8 + 64 + 64 + 26
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprintf("cap%+d", delta), func(t *testing.T) {
			records := quotaSizedRecords(t, MaxScopedDaemonBytes+delta, overhead, backing, func(i int, b []byte) ImportRecord {
				return &ScopedBatchExport{ViewUID: quotaUID(1), Generation: 1, Sequence: int64(i + 1), Digest: fmt.Sprintf("%064x", i+1), PreviousDigest: quotaDigest, CaptureUID: quotaUID(i + 100), BatchBytes: b}
			})
			// Native rows are outside this new namespace's quota, even at the exact cap.
			records = append(records, &MetaKV{Key: "example", Value: nativeValue})
			err := (&scopedImportGraph{}).validateQuotas(records)
			if delta > 0 {
				require.ErrorContains(t, err, "scoped aggregate retained-byte quota exceeded")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func quotaComponent(g *scopedImportGraph, start, count int, totalBodies int64, backing []byte) {
	for i := 0; i < count; i++ {
		id := quotaUID(start + i)
		g.operations[id] = &ScopedSourceOperationExport{OperationUID: id}
		n := totalBodies / int64(count)
		if int64(i) < totalBodies%int64(count) {
			n++
		}
		g.submissions[id] = SourceSubmission{Operation: SourceOperation{UID: id}, OriginalBytes: [][]byte{backing[:int(n)]}}
		if i > 0 {
			prior := quotaUID(start + i - 1)
			g.dependencies[scopedDependencyKey{id, quotaUID(9005), quotaUID(i + 1000)}] = &ScopedSourceDependencyExport{OperationUID: id, EstablishingOperationUID: &prior}
		}
	}
	// Multiple paths and a cycle must not count the same operation/body twice.
	if count > 2 {
		first, last := quotaUID(start), quotaUID(start+count-1)
		g.dependencies[scopedDependencyKey{first, quotaUID(9005), quotaUID(9006)}] = &ScopedSourceDependencyExport{OperationUID: first, EstablishingOperationUID: &last}
	}
}
func quotaComponentGraph() scopedImportGraph {
	return scopedImportGraph{operations: map[string]*ScopedSourceOperationExport{}, submissions: map[string]SourceSubmission{}, dependencies: map[scopedDependencyKey]*ScopedSourceDependencyExport{}}
}

func TestScopedImportQuotaDependencyComponents(t *testing.T) {
	backing := make([]byte, 1<<20)
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("operations_cap%+d", delta), func(t *testing.T) {
			g := quotaComponentGraph()
			quotaComponent(&g, 100, MaxScopedComponentOperations+delta, 0, backing)
			err := g.validateQuotas(nil)
			if delta > 0 {
				require.ErrorContains(t, err, "scoped dependency component exceeds retained source bounds")
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprintf("body_bytes_cap%+d", delta), func(t *testing.T) {
			g := quotaComponentGraph()
			quotaComponent(&g, 100, 8, MaxScopedComponentOriginalBytes+delta, backing)
			err := g.validateQuotas(nil)
			if delta > 0 {
				require.ErrorContains(t, err, "scoped dependency component exceeds retained source bounds")
			} else {
				require.NoError(t, err)
			}
		})
	}
	t.Run("disconnected_at_caps_with_shared_visits", func(t *testing.T) {
		g := quotaComponentGraph()
		quotaComponent(&g, 100, MaxScopedComponentOperations, MaxScopedComponentOriginalBytes, backing)
		quotaComponent(&g, 1000, MaxScopedComponentOperations, MaxScopedComponentOriginalBytes, backing)
		require.NoError(t, g.validateQuotas(nil), "each disconnected component independently fits; undirected/cyclic visits count each original once")
	})
}
