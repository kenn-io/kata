package daemon

import (
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"go.kenn.io/kata/internal/api"
)

func TestMarkTransitiveBlockLayoutScaling(t *testing.T) {
	for _, tc := range []struct {
		shape        string
		small, large int
	}{
		{"blocks hub", 500, 4000},
		{"hub with back-link", 1000, 8000},
		{"hub fan-in/out", 1000, 8000},
		{"layered10", 250, 2000},
		{"skip4", 1000, 8000},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			var timings []time.Duration
			for _, size := range []int{tc.small, tc.large} {
				edges := layoutShape(tc.shape, size)
				markTransitiveBlockLayout(edges)
				var samples []time.Duration
				for range 3 {
					// Discard garbage from the preceding graph before timing.
					runtime.GC()
					start := time.Now()
					markTransitiveBlockLayout(edges)
					samples = append(samples, time.Since(start))
				}
				slices.Sort(samples)
				timings = append(timings, samples[1])
			}
			t.Logf("%d=%v, %d=%v", tc.small, timings[0], tc.large, timings[1])
			exponent := 1.5
			if tc.shape == "layered10" || tc.shape == "skip4" {
				// Reachability rows grow with both tasks and bitset words.
				exponent = 2
			}
			require.Less(t, float64(timings[1]), float64(timings[0])*math.Pow(float64(tc.large)/float64(tc.small), exponent))
		})
	}
}

func TestMarkTransitiveBlockLayoutOracleEquivalence(t *testing.T) {
	check := func(t interface{ Fatalf(string, ...any) }, edges, want []api.ReachableGraphEdge) {
		expected := layoutOracle(edges)
		if want != nil && !slices.Equal(want, expected) {
			t.Fatalf("edges=%v: literal expectation %v, oracle %v", edges, want, expected)
		}
		actual := slices.Clone(edges)
		markTransitiveBlockLayout(actual)
		if !slices.Equal(expected, actual) {
			t.Fatalf("edges=%v: expected %v, got %v", edges, expected, actual)
		}
	}

	t.Run("shapes", func(t *testing.T) {
		for _, shape := range []string{"blocks hub", "hub with back-link", "related hub", "hub fan-in/out", "layered10", "skip4"} {
			t.Run(shape, func(t *testing.T) {
				edges := layoutShape(shape, 60)
				want := slices.Clone(edges)
				if shape == "skip4" {
					for i := range want {
						from, _ := strconv.Atoi(want[i].FromUID)
						to, _ := strconv.Atoi(want[i].ToUID)
						want[i].Layout = to-from == 1
					}
				}
				check(t, edges, want)
			})
		}
	})

	t.Run("exhaustive", func(t *testing.T) {
		var slots []api.ReachableGraphEdge
		for from := range 4 {
			for to := range 4 {
				if from != to {
					slots = append(slots, api.ReachableGraphEdge{FromUID: strconv.Itoa(from), ToUID: strconv.Itoa(to), Kind: "blocks", Layout: true})
				}
			}
		}
		for mask := range 1 << len(slots) {
			var edges []api.ReachableGraphEdge
			for i, edge := range slots {
				if mask&(1<<i) != 0 {
					edges = append(edges, edge)
				}
			}
			check(t, edges, nil)
		}
	})

	t.Run("multiword", func(t *testing.T) {
		random := rand.New(rand.NewPCG(12345, 67890)) //nolint:gosec // G404: reproducible graph fixture.
		var edges []api.ReachableGraphEdge
		// 130 join targets span three uint64 words; nearby links guarantee joins.
		for from := range 132 {
			for to := from + 1; to < 132; to++ {
				if to-from <= 2 || random.Float64() < 0.2 {
					edges = append(edges, api.ReachableGraphEdge{FromUID: strconv.Itoa(from), ToUID: strconv.Itoa(to), Kind: "blocks", Layout: true})
				}
			}
		}
		random.Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })
		check(t, edges, nil)
	})

	t.Run("property", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			count := rapid.IntRange(1, 12).Draw(t, "count")
			var edges []api.ReachableGraphEdge
			for from := range count {
				for to := range count {
					if from == to {
						continue
					}
					kind := rapid.SampledFrom([]string{"", "", "", "blocks", "blocks", "parent", "related"}).Draw(t, fmt.Sprintf("%d->%d", from, to))
					if kind != "" && (kind != "related" || from < to) {
						edges = append(edges, api.ReachableGraphEdge{FromUID: strconv.Itoa(from), ToUID: strconv.Itoa(to), Kind: kind, Layout: true})
					}
				}
			}
			check(t, edges, nil)
		})
	})
}

func layoutOracle(edges []api.ReachableGraphEdge) []api.ReachableGraphEdge {
	adj := map[string][]string{}
	for _, edge := range edges {
		if edge.Kind == "blocks" {
			adj[edge.FromUID] = append(adj[edge.FromUID], edge.ToUID)
		}
	}
	for uid := range adj {
		slices.Sort(adj[uid])
	}
	expected := slices.Clone(edges)
	for i, edge := range expected {
		if edge.Kind == "blocks" && hasAlternateBlockPathOracle(adj, edge.FromUID, edge.ToUID) {
			expected[i].Layout = false
		}
	}
	return expected
}

// Keep the original per-edge BFS as an independent oracle.
func hasAlternateBlockPathOracle(adj map[string][]string, from, to string) bool {
	seen := map[string]struct{}{from: {}}
	queue := append([]string(nil), adj[from]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == to {
			continue
		}
		if _, ok := seen[next]; ok {
			continue
		}
		seen[next] = struct{}{}
		for _, peer := range adj[next] {
			if peer == to {
				return true
			}
			if _, ok := seen[peer]; !ok {
				queue = append(queue, peer)
			}
		}
	}
	return false
}

func layoutShape(shape string, size int) []api.ReachableGraphEdge {
	var edges []api.ReachableGraphEdge
	connect := func(from, to int) {
		kind := "blocks"
		if shape == "related hub" {
			kind = "related"
		}
		edges = append(edges, api.ReachableGraphEdge{FromUID: strconv.Itoa(from), ToUID: strconv.Itoa(to), Kind: kind, Layout: true})
	}
	for index := 1; index < size; index++ {
		switch shape {
		case "blocks hub", "related hub", "hub with back-link":
			connect(0, index)
		case "hub fan-in/out":
			if index < size/2 {
				connect(index, 0)
			} else {
				connect(0, index)
			}
		case "layered10":
			previousLayer := (index/10 - 1) * 10
			if previousLayer >= 0 {
				for peer := previousLayer; peer < previousLayer+10; peer++ {
					connect(peer, index)
				}
			}
		case "skip4":
			for skip := 1; skip <= 4 && skip <= index; skip++ {
				connect(index-skip, index)
			}
		}
	}
	if shape == "hub with back-link" {
		connect(1, 0)
	}
	return edges
}
