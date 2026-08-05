package phase2

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestComponentsBuildsTransitiveAndDisjointSortedGroups(t *testing.T) {
	a, b, c, d, e := testSHA('a'), testSHA('b'), testSHA('c'), testSHA('d'), testSHA('e')
	got, err := Components([][2]string{
		{c, b},
		{e, d},
		{b, a},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{a, b, c}, {d, e}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Components = %#v, want %#v", got, want)
	}
}

func TestComponentsNormalizesDuplicateReverseAndSelfEdges(t *testing.T) {
	a, b, c := testSHA('a'), testSHA('b'), testSHA('c')
	got, err := Components([][2]string{
		{b, a},
		{a, b},
		{b, a},
		{a, a},
		{c, c},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{a, b}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Components = %#v, want %#v", got, want)
	}
}

func TestComponentsRejectsNoncanonicalEndpoints(t *testing.T) {
	valid := testSHA('a')
	for _, invalid := range []string{
		"",
		strings.Repeat("a", 127),
		strings.Repeat("a", 129),
		strings.Repeat("A", 128),
		strings.Repeat("g", 128),
	} {
		t.Run(fmt.Sprintf("%q", invalid), func(t *testing.T) {
			if _, err := Components([][2]string{{valid, invalid}}); err == nil {
				t.Fatal("Components accepted a noncanonical SHA endpoint")
			}
		})
	}
}

func TestComponentsRejectsInvalidSelfEdgeBeforeIgnoringIt(t *testing.T) {
	invalid := strings.Repeat("A", 128)
	if _, err := Components([][2]string{{invalid, invalid}}); err == nil {
		t.Fatal("Components ignored a noncanonical self-edge without validating it")
	}
}

func TestComponentsDoesNotModifyInputEdges(t *testing.T) {
	a, b, c := testSHA('a'), testSHA('b'), testSHA('c')
	edges := [][2]string{{c, b}, {b, a}, {a, c}}
	before := append([][2]string(nil), edges...)
	if _, err := Components(edges); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edges, before) {
		t.Fatalf("Components modified input:\ngot=%#v\nwant=%#v", edges, before)
	}
}

func TestComponentsIsIndependentOfInputOrderDirectionAndUnionRoots(t *testing.T) {
	a, b, c, d, e, f := testSHA('1'), testSHA('2'), testSHA('3'), testSHA('4'), testSHA('5'), testSHA('6')
	base := [][2]string{
		{a, b},
		{b, c},
		{d, e},
		{e, f},
		{a, c},
	}
	want := [][]string{{a, b, c}, {d, e, f}}
	rng := rand.New(rand.NewSource(20260728))
	for iteration := 0; iteration < 100; iteration++ {
		edges := append([][2]string(nil), base...)
		rng.Shuffle(len(edges), func(i, j int) {
			edges[i], edges[j] = edges[j], edges[i]
		})
		for index := range edges {
			if rng.Intn(2) == 0 {
				edges[index][0], edges[index][1] = edges[index][1], edges[index][0]
			}
		}
		got, err := Components(edges)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d Components = %#v, want %#v", iteration, got, want)
		}
	}
}

func TestComponentsRandomizedMatchesIndependentNaiveOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(803471))
	nodes := []string{
		testSHA('0'), testSHA('1'), testSHA('2'), testSHA('3'),
		testSHA('4'), testSHA('5'), testSHA('6'), testSHA('7'),
	}

	for iteration := 0; iteration < 250; iteration++ {
		var edges [][2]string
		edgeCount := rng.Intn(30)
		for edgeIndex := 0; edgeIndex < edgeCount; edgeIndex++ {
			left := nodes[rng.Intn(len(nodes))]
			right := nodes[rng.Intn(len(nodes))]
			edge := [2]string{left, right}
			edges = append(edges, edge)
			if rng.Intn(4) == 0 {
				edges = append(edges, [2]string{right, left})
			}
			if rng.Intn(5) == 0 {
				edges = append(edges, edge)
			}
		}

		got, err := Components(edges)
		if err != nil {
			t.Fatal(err)
		}
		want := naiveComponents(edges)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d:\nedges=%#v\ngot=%#v\nwant=%#v", iteration, edges, got, want)
		}
	}
}

func naiveComponents(edges [][2]string) [][]string {
	adjacent := make(map[string]map[string]struct{})
	for _, edge := range edges {
		left, right := edge[0], edge[1]
		if left == right {
			continue
		}
		if adjacent[left] == nil {
			adjacent[left] = make(map[string]struct{})
		}
		if adjacent[right] == nil {
			adjacent[right] = make(map[string]struct{})
		}
		adjacent[left][right] = struct{}{}
		adjacent[right][left] = struct{}{}
	}

	var nodes []string
	for node := range adjacent {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	seen := make(map[string]bool)
	var result [][]string
	for _, start := range nodes {
		if seen[start] {
			continue
		}
		seen[start] = true
		queue := []string{start}
		var members []string
		for len(queue) > 0 {
			node := queue[0]
			queue = queue[1:]
			members = append(members, node)
			for neighbor := range adjacent[node] {
				if !seen[neighbor] {
					seen[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		sort.Strings(members)
		if len(members) >= 2 {
			result = append(result, members)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return compareMembers(result[i], result[j]) < 0
	})
	return result
}

func compareMembers(left, right []string) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	default:
		return 0
	}
}

func testSHA(fill byte) string {
	return strings.Repeat(string(fill), 128)
}
