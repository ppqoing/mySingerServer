package localanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type GroupFile struct {
	FileID  int64
	SHA512  string
	Path    string
	Quality int
}

type PairDecision struct {
	Category string
	SHAA     string
	SHAB     string
	Verdict  string
}

type FinalGroup struct {
	GroupID              string
	Category             string
	RepresentativeFileID int64
	Members              []GroupFile
}

func BuildFinalGroups(runID string, files []GroupFile, decisions []PairDecision) ([]FinalGroup, error) {
	if runID == "" {
		return nil, fmt.Errorf("localanalysis: empty run ID")
	}
	bySHA := make(map[string][]GroupFile)
	seen := make(map[int64]struct{}, len(files))
	for _, file := range files {
		if file.FileID <= 0 || file.SHA512 == "" {
			return nil, fmt.Errorf("localanalysis: invalid group file")
		}
		if _, exists := seen[file.FileID]; exists {
			return nil, fmt.Errorf("localanalysis: duplicate file ID %d", file.FileID)
		}
		seen[file.FileID] = struct{}{}
		bySHA[file.SHA512] = append(bySHA[file.SHA512], file)
	}
	var groups []FinalGroup
	for sha, members := range bySHA {
		if len(members) >= 2 {
			groups = append(groups, makeFinalGroup(runID, "exact", []string{sha}, members))
		}
	}
	for _, category := range []string{"image", "video"} {
		parent := make(map[string]string)
		var find func(string) string
		find = func(value string) string {
			if parent[value] == "" {
				parent[value] = value
			}
			if parent[value] != value {
				parent[value] = find(parent[value])
			}
			return parent[value]
		}
		for _, edge := range decisions {
			if edge.Verdict != "yes" || edge.Category != category {
				continue
			}
			if edge.SHAA == "" || edge.SHAB == "" || edge.SHAA == edge.SHAB {
				return nil, fmt.Errorf("localanalysis: invalid confirmed pair")
			}
			if len(bySHA[edge.SHAA]) == 0 || len(bySHA[edge.SHAB]) == 0 {
				return nil, fmt.Errorf("localanalysis: confirmed pair file is missing")
			}
			a, b := find(edge.SHAA), find(edge.SHAB)
			if a != b {
				if b < a {
					a, b = b, a
				}
				parent[b] = a
			}
		}
		components := make(map[string][]string)
		for sha := range parent {
			root := find(sha)
			components[root] = append(components[root], sha)
		}
		for _, shas := range components {
			if len(shas) < 2 {
				continue
			}
			sort.Strings(shas)
			var members []GroupFile
			for _, sha := range shas {
				members = append(members, bySHA[sha]...)
			}
			groups = append(groups, makeFinalGroup(runID, category, shas, members))
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Category != groups[j].Category {
			return groups[i].Category < groups[j].Category
		}
		return groups[i].GroupID < groups[j].GroupID
	})
	return groups, nil
}

func makeFinalGroup(runID, category string, shas []string, members []GroupFile) FinalGroup {
	sort.Slice(members, func(i, j int) bool {
		if members[i].SHA512 != members[j].SHA512 {
			return members[i].SHA512 < members[j].SHA512
		}
		if members[i].Path != members[j].Path {
			return members[i].Path < members[j].Path
		}
		return members[i].FileID < members[j].FileID
	})
	representative := members[0]
	for _, file := range members[1:] {
		if file.Quality > representative.Quality ||
			(file.Quality == representative.Quality && (file.SHA512 < representative.SHA512 ||
				(file.SHA512 == representative.SHA512 && (file.Path < representative.Path ||
					(file.Path == representative.Path && file.FileID < representative.FileID))))) {
			representative = file
		}
	}
	sortedSHAs := append([]string(nil), shas...)
	sort.Strings(sortedSHAs)
	sum := sha256.Sum256([]byte(runID + "\x00" + category + "\x00" + strings.Join(sortedSHAs, "\x00")))
	return FinalGroup{
		GroupID: "local-" + hex.EncodeToString(sum[:16]), Category: category,
		RepresentativeFileID: representative.FileID, Members: append([]GroupFile(nil), members...),
	}
}
