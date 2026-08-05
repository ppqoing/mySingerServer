package phase2

import (
	"fmt"
	"sort"
)

type unionFind struct {
	parent map[string]string
	rank   map[string]uint
}

func newUnionFind() *unionFind {
	return &unionFind{
		parent: make(map[string]string),
		rank:   make(map[string]uint),
	}
}

func (set *unionFind) find(value string) string {
	parent, exists := set.parent[value]
	if !exists {
		set.parent[value] = value
		set.rank[value] = 0
		return value
	}
	if parent != value {
		set.parent[value] = set.find(parent)
	}
	return set.parent[value]
}

func (set *unionFind) union(left, right string) {
	leftRoot := set.find(left)
	rightRoot := set.find(right)
	if leftRoot == rightRoot {
		return
	}
	if set.rank[leftRoot] < set.rank[rightRoot] {
		leftRoot, rightRoot = rightRoot, leftRoot
	}
	set.parent[rightRoot] = leftRoot
	if set.rank[leftRoot] == set.rank[rightRoot] {
		set.rank[leftRoot]++
	}
}

// Components returns deterministic connected components for canonical
// undirected SHA-512 edges. Singleton self-edges are not duplicate groups.
func Components(edges [][2]string) ([][]string, error) {
	normalizedSet := make(map[[2]string]struct{}, len(edges))
	for index, edge := range edges {
		left, right := edge[0], edge[1]
		if !isCanonicalSHA512(left) {
			return nil, fmt.Errorf("phase2: edge %d has noncanonical left SHA-512", index)
		}
		if !isCanonicalSHA512(right) {
			return nil, fmt.Errorf("phase2: edge %d has noncanonical right SHA-512", index)
		}
		if left == right {
			continue
		}
		if right < left {
			left, right = right, left
		}
		normalizedSet[[2]string{left, right}] = struct{}{}
	}

	normalized := make([][2]string, 0, len(normalizedSet))
	for edge := range normalizedSet {
		normalized = append(normalized, edge)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i][0] != normalized[j][0] {
			return normalized[i][0] < normalized[j][0]
		}
		return normalized[i][1] < normalized[j][1]
	})

	set := newUnionFind()
	for _, edge := range normalized {
		set.union(edge[0], edge[1])
	}

	byRoot := make(map[string][]string)
	for value := range set.parent {
		root := set.find(value)
		byRoot[root] = append(byRoot[root], value)
	}
	components := make([][]string, 0, len(byRoot))
	for _, members := range byRoot {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		components = append(components, members)
	}
	if len(components) == 0 {
		return nil, nil
	}
	sort.Slice(components, func(i, j int) bool {
		return compareComponentMembers(components[i], components[j]) < 0
	})
	return components, nil
}

func compareComponentMembers(left, right []string) int {
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
