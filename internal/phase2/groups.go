package phase2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrGroupCommitOutcomeUnknown marks a Commit error for which PostgreSQL may
// have committed even though the client did not receive the acknowledgement.
// Retrying the same whole-kind rebuild converges to the same semantic result.
var ErrGroupCommitOutcomeUnknown = errors.New(
	"phase2: group rebuild commit outcome unknown",
)

// GroupStats reports only confirmed writes from a successful Commit.
type GroupStats struct {
	Groups  int
	Members int
}

// GroupRebuilder transactionally replaces one confirmed M4 group kind.
type GroupRebuilder struct {
	db groupRebuildDB
}

type groupRebuildDB interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// NewGroupRebuilder constructs the production PostgreSQL-backed rebuilder.
func NewGroupRebuilder(db groupRebuildDB) *GroupRebuilder {
	return &GroupRebuilder{db: db}
}

type confirmedEdge struct {
	Key        PairKey
	Similarity float64
	Detail     json.RawMessage
}

type confirmedFile struct {
	ID        int64
	SHA       string
	MachineID string
	Path      string
	Quality   int
}

type confirmedGroup struct {
	Representative confirmedFile
	Members        []confirmedFile
}

type memberEdgeJSON struct {
	SHAA string `json:"sha_a"`
	SHAB string `json:"sha_b"`
}

type memberScoreJSON struct {
	Role     string          `json:"role"`
	VSRepSHA string          `json:"vs_rep_sha,omitempty"`
	Via      bool            `json:"via,omitempty"`
	Edge     *memberEdgeJSON `json:"edge,omitempty"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

// RebuildGroups replaces all confirmed groups for kind. Only image and video
// are valid kinds.
func (rebuilder *GroupRebuilder) RebuildGroups(
	ctx context.Context,
	kind string,
) (stats GroupStats, err error) {
	if kind != "image" && kind != "video" {
		return GroupStats{}, fmt.Errorf(
			"phase2: invalid confirmed group kind %q",
			kind,
		)
	}
	if rebuilder == nil || rebuilder.db == nil {
		return GroupStats{}, fmt.Errorf("phase2: group rebuilder PostgreSQL DB is nil")
	}

	tx, err := rebuilder.db.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead,
	})
	if err != nil {
		return GroupStats{}, fmt.Errorf("phase2: begin group rebuild: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			5*time.Second,
		)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if _, err = tx.Exec(ctx, `
		LOCK TABLE dup_groups IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return GroupStats{}, fmt.Errorf(
			"phase2: lock group replacement domain: %w",
			err,
		)
	}
	edges, components, err := loadConfirmedEdges(ctx, tx, kind)
	if err != nil {
		return GroupStats{}, err
	}
	groups, err := loadConfirmedGroups(ctx, tx, kind, components)
	if err != nil {
		return GroupStats{}, err
	}
	if _, err = tx.Exec(
		ctx,
		`DELETE FROM dup_groups WHERE kind=$1`,
		kind,
	); err != nil {
		return GroupStats{}, fmt.Errorf(
			"phase2: delete old %s groups: %w",
			kind,
			err,
		)
	}

	edgeByKey := make(map[PairKey]confirmedEdge, len(edges))
	for _, edge := range edges {
		edgeByKey[edge.Key] = edge
	}
	for _, group := range groups {
		var groupID int64
		err = tx.QueryRow(ctx, `
			INSERT INTO dup_groups(
				kind,representative_file_id,member_count,created_at
			)
			VALUES($1,$2,$3,now())
			RETURNING id`,
			kind,
			group.Representative.ID,
			len(group.Members),
		).Scan(&groupID)
		if err != nil {
			return GroupStats{}, fmt.Errorf(
				"phase2: insert %s group: %w",
				kind,
				err,
			)
		}
		for _, member := range group.Members {
			raw, marshalErr := marshalMemberScore(
				member,
				group.Representative,
				edges,
				edgeByKey,
			)
			if marshalErr != nil {
				return GroupStats{}, marshalErr
			}
			if _, err = tx.Exec(ctx, `
				INSERT INTO dup_members(group_id,file_id,score_json)
				VALUES($1,$2,$3::jsonb)`,
				groupID,
				member.ID,
				raw,
			); err != nil {
				return GroupStats{}, fmt.Errorf(
					"phase2: insert %s group member: %w",
					kind,
					err,
				)
			}
		}
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		if errors.Is(commitErr, pgx.ErrTxCommitRollback) {
			return GroupStats{}, fmt.Errorf(
				"phase2: commit group rebuild rolled back: %w",
				commitErr,
			)
		}
		return GroupStats{}, errors.Join(
			ErrGroupCommitOutcomeUnknown,
			fmt.Errorf("phase2: commit group rebuild: %w", commitErr),
		)
	}
	return GroupStats{
		Groups:  len(groups),
		Members: groupMemberCount(groups),
	}, nil
}

func loadConfirmedEdges(
	ctx context.Context,
	tx pgx.Tx,
	kind string,
) ([]confirmedEdge, [][]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT kind,sha_a,sha_b,phase2_json,verdict
		FROM pair_scores
		WHERE kind=$1
		ORDER BY sha_a,sha_b`,
		kind,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"phase2: query %s pair scores: %w",
			kind,
			err,
		)
	}
	defer rows.Close()

	var (
		edges          []confirmedEdge
		componentEdges [][2]string
	)
	seen := make(map[PairKey]struct{})
	for rows.Next() {
		var (
			key     PairKey
			raw     []byte
			verdict string
		)
		if err := rows.Scan(
			&key.Kind,
			&key.SHAA,
			&key.SHAB,
			&raw,
			&verdict,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"phase2: scan %s pair score: %w",
				kind,
				err,
			)
		}
		if key.Kind != kind {
			return nil, nil, fmt.Errorf(
				"phase2: requested %s score query returned kind %q",
				kind,
				key.Kind,
			)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, nil, fmt.Errorf(
				"phase2: duplicate requested-kind pair score %#v",
				key,
			)
		}
		seen[key] = struct{}{}
		if len(raw) == 0 {
			return nil, nil, fmt.Errorf(
				"phase2: pair %#v has null phase2_json",
				key,
			)
		}
		var identity pairScoreDocument
		if err := decodeStrictJSON(raw, &identity); err != nil {
			return nil, nil, fmt.Errorf(
				"phase2: decode pair %#v: %w",
				key,
				err,
			)
		}
		trace, err := traceFromScoreDocument(key.Kind, identity.FirstScreen)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"phase2: pair %#v trace: %w",
				key,
				err,
			)
		}
		score, err := decodePersistedPairScore(raw, key, verdict, trace)
		if err != nil {
			return nil, nil, err
		}
		if verdict != "yes" {
			continue
		}
		similarity, err := confirmedFinalSimilarity(score)
		if err != nil {
			return nil, nil, err
		}
		edge := confirmedEdge{
			Key:        key,
			Similarity: similarity,
			Detail:     append(json.RawMessage(nil), raw...),
		}
		edges = append(edges, edge)
		componentEdges = append(
			componentEdges,
			[2]string{key.SHAA, key.SHAB},
		)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"phase2: read %s pair scores: %w",
			kind,
			err,
		)
	}
	components, err := Components(componentEdges)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"phase2: build %s components: %w",
			kind,
			err,
		)
	}
	return edges, components, nil
}

func confirmedFinalSimilarity(score persistedPairScore) (float64, error) {
	switch score.Key.Kind {
	case "image":
		if score.Document.Image == nil ||
			!score.Document.Image.SobelEvaluated ||
			score.Document.Image.SobelCosine == nil {
			return 0, fmt.Errorf(
				"phase2: yes image pair %#v lacks final similarity",
				score.Key,
			)
		}
		return *score.Document.Image.SobelCosine, nil
	case "video":
		if score.Document.Video == nil ||
			!score.Document.Video.AverageEvaluated ||
			score.Document.Video.Average == nil {
			return 0, fmt.Errorf(
				"phase2: yes video pair %#v lacks final similarity",
				score.Key,
			)
		}
		return *score.Document.Video.Average, nil
	default:
		return 0, fmt.Errorf(
			"phase2: pair %#v has invalid confirmed kind",
			score.Key,
		)
	}
}

func loadConfirmedGroups(
	ctx context.Context,
	tx pgx.Tx,
	kind string,
	components [][]string,
) ([]confirmedGroup, error) {
	shaSet := make(map[string]struct{})
	for _, component := range components {
		for _, sha := range component {
			shaSet[sha] = struct{}{}
		}
	}
	if len(shaSet) == 0 {
		return nil, nil
	}
	shas := sortedSet(shaSet)
	rows, err := tx.Query(ctx, `
		SELECT
			f.id,
			f.sha512,
			f.machine_id,
			f.path,
			CASE
				WHEN $2='image' THEN COALESCE(image.pdq_quality,0)
				ELSE COALESCE(video.thumb_quality,0)
			END AS quality
		FROM files AS f
		LEFT JOIN image_features AS image ON image.sha512=f.sha512
		LEFT JOIN video_features AS video ON video.sha512=f.sha512
		WHERE f.sha512=ANY($1::text[])
		  AND f.status <> 'deleted'
		ORDER BY f.sha512,f.machine_id,f.path,f.id`,
		shas,
		kind,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"phase2: query live %s component files: %w",
			kind,
			err,
		)
	}
	defer rows.Close()
	filesBySHA := make(map[string][]confirmedFile)
	seenIDs := make(map[int64]struct{})
	for rows.Next() {
		var file confirmedFile
		if err := rows.Scan(
			&file.ID,
			&file.SHA,
			&file.MachineID,
			&file.Path,
			&file.Quality,
		); err != nil {
			return nil, fmt.Errorf(
				"phase2: scan live %s component file: %w",
				kind,
				err,
			)
		}
		if file.ID <= 0 {
			return nil, fmt.Errorf(
				"phase2: live %s component file has invalid ID %d",
				kind,
				file.ID,
			)
		}
		if !isCanonicalSHA512(file.SHA) {
			return nil, fmt.Errorf(
				"phase2: live %s component file has noncanonical SHA %q",
				kind,
				file.SHA,
			)
		}
		if _, expected := shaSet[file.SHA]; !expected {
			return nil, fmt.Errorf(
				"phase2: live %s query returned unrelated SHA %q",
				kind,
				file.SHA,
			)
		}
		if _, duplicate := seenIDs[file.ID]; duplicate {
			return nil, fmt.Errorf(
				"phase2: live %s query returned duplicate file ID %d",
				kind,
				file.ID,
			)
		}
		seenIDs[file.ID] = struct{}{}
		filesBySHA[file.SHA] = append(filesBySHA[file.SHA], file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"phase2: read live %s component files: %w",
			kind,
			err,
		)
	}

	groups := make([]confirmedGroup, 0, len(components))
	for _, component := range components {
		liveKeys := 0
		var members []confirmedFile
		for _, sha := range component {
			copies := filesBySHA[sha]
			if len(copies) == 0 {
				continue
			}
			liveKeys++
			members = append(members, copies...)
		}
		if liveKeys < 2 {
			continue
		}
		sortConfirmedFiles(members)
		representative := members[0]
		for _, member := range members[1:] {
			if betterRepresentative(member, representative) {
				representative = member
			}
		}
		groups = append(groups, confirmedGroup{
			Representative: representative,
			Members:        members,
		})
	}
	return groups, nil
}

func sortConfirmedFiles(files []confirmedFile) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].SHA != files[j].SHA {
			return files[i].SHA < files[j].SHA
		}
		if files[i].MachineID != files[j].MachineID {
			return files[i].MachineID < files[j].MachineID
		}
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		return files[i].ID < files[j].ID
	})
}

func betterRepresentative(candidate, current confirmedFile) bool {
	if candidate.Quality != current.Quality {
		return candidate.Quality > current.Quality
	}
	if candidate.MachineID != current.MachineID {
		return candidate.MachineID < current.MachineID
	}
	if candidate.Path != current.Path {
		return candidate.Path < current.Path
	}
	return candidate.ID < current.ID
}

func marshalMemberScore(
	member confirmedFile,
	representative confirmedFile,
	edges []confirmedEdge,
	edgeByKey map[PairKey]confirmedEdge,
) ([]byte, error) {
	document := memberScoreJSON{Role: "representative"}
	if member.ID != representative.ID {
		document.Role = "member"
		document.VSRepSHA = representative.SHA
		if member.SHA != representative.SHA {
			key := normalizedConfirmedKey(
				edgeKind(edges),
				member.SHA,
				representative.SHA,
			)
			edge, direct := edgeByKey[key]
			if !direct {
				var found bool
				edge, found = bestIncidentEdge(member.SHA, edges)
				if !found {
					return nil, fmt.Errorf(
						"phase2: member SHA %s has no passing component edge",
						member.SHA,
					)
				}
				document.Via = true
			}
			document.Edge = &memberEdgeJSON{
				SHAA: edge.Key.SHAA,
				SHAB: edge.Key.SHAB,
			}
			document.Detail = append(json.RawMessage(nil), edge.Detail...)
		}
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf(
			"phase2: marshal member %d score: %w",
			member.ID,
			err,
		)
	}
	return raw, nil
}

func bestIncidentEdge(
	sha string,
	edges []confirmedEdge,
) (confirmedEdge, bool) {
	var best confirmedEdge
	found := false
	for _, edge := range edges {
		if edge.Key.SHAA != sha && edge.Key.SHAB != sha {
			continue
		}
		if !found ||
			edge.Similarity > best.Similarity ||
			(edge.Similarity == best.Similarity &&
				lessPairKey(edge.Key, best.Key)) {
			best = edge
			found = true
		}
	}
	return best, found
}

func normalizedConfirmedKey(kind, left, right string) PairKey {
	if right < left {
		left, right = right, left
	}
	return PairKey{Kind: kind, SHAA: left, SHAB: right}
}

func edgeKind(edges []confirmedEdge) string {
	if len(edges) == 0 {
		return ""
	}
	return edges[0].Key.Kind
}

func lessPairKey(left, right PairKey) bool {
	if left.SHAA != right.SHAA {
		return left.SHAA < right.SHAA
	}
	return left.SHAB < right.SHAB
}

func groupMemberCount(groups []confirmedGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.Members)
	}
	return total
}
