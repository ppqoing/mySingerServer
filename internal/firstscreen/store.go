package firstscreen

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrCommitOutcomeUnknown marks a Commit error for which the server may have
// committed even though the client did not receive a successful response.
// Callers reconcile by retrying the same whole-class replacement.
var ErrCommitOutcomeUnknown = errors.New("firstscreen: commit outcome unknown")

const (
	qFilesBySHASet = `
		SELECT id,sha512,machine_id,disk_no,path,size
		FROM files
		WHERE sha512 = ANY($1::text[])
		ORDER BY sha512,id`
	qDeleteMembersM3 = `
		DELETE FROM dup_members
		WHERE group_id IN (
			SELECT id FROM dup_groups WHERE kind = ANY($1::text[])
		)`
	qDeleteGroupsM3 = `
		DELETE FROM dup_groups
		WHERE kind = ANY($1::text[])`
	qInsertGroup = `
		INSERT INTO dup_groups(
			kind,representative_file_id,member_count,created_at
		)
		VALUES($1,$2,$3,now())
		RETURNING id`
)

type Store struct {
	conn    storeConn
	cfg     Config
	badRows int
}

type storeConn interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func NewStore(conn *pgx.Conn, cfg Config) *Store {
	return &Store{conn: conn, cfg: cfg}
}

func (s *Store) BadRows() int {
	return s.badRows
}

func (s *Store) LoadImageFeatures(ctx context.Context) ([]ImageFeature, error) {
	var (
		after    pgtype.Text
		features []ImageFeature
	)
	for {
		rows, err := s.conn.Query(ctx, `
			SELECT sha512,width,height,pdq256,pdq_quality
			FROM image_features
			WHERE ($1::text IS NULL OR sha512 > $1::text)
			  AND pdq256 IS NOT NULL
			  AND pdq_quality >= $2
			ORDER BY sha512
			LIMIT $3`,
			after, s.cfg.ImageQualityMin, s.cfg.ReadPageSize)
		if err != nil {
			return nil, fmt.Errorf("query image_features: %w", err)
		}

		pageRows := 0
		for rows.Next() {
			var (
				shaText       string
				width, height int
				pdqBytes      []byte
				quality       int
			)
			if err := rows.Scan(&shaText, &width, &height, &pdqBytes, &quality); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan image_features: %w", err)
			}
			pageRows++
			after = pgtype.Text{String: shaText, Valid: true}
			sha, shaOK := shaFromText(shaText)
			pdq, pdqOK := pdqFromBytes(pdqBytes)
			if !shaOK || !pdqOK {
				s.badRows++
				continue
			}
			features = append(features, ImageFeature{
				SHA512:  sha,
				PDQ:     pdq,
				Quality: quality,
				Width:   width,
				Height:  height,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read image_features: %w", err)
		}
		rows.Close()
		if pageRows < s.cfg.ReadPageSize {
			return features, nil
		}
	}
}

func (s *Store) LoadVideoFeatures(ctx context.Context) ([]VideoFeature, error) {
	var (
		after    pgtype.Text
		features []VideoFeature
	)
	for {
		rows, err := s.conn.Query(ctx, `
			SELECT sha512,duration_ms,thumb_pdq256,thumb_quality
			FROM video_features
			WHERE ($1::text IS NULL OR sha512 > $1::text)
			  AND thumb_pdq256 IS NOT NULL
			  AND duration_ms IS NOT NULL
			ORDER BY sha512
			LIMIT $2`,
			after, s.cfg.ReadPageSize)
		if err != nil {
			return nil, fmt.Errorf("query video_features: %w", err)
		}

		pageRows := 0
		for rows.Next() {
			var (
				shaText  string
				duration int64
				pdqBytes []byte
				quality  pgtype.Int4
			)
			if err := rows.Scan(&shaText, &duration, &pdqBytes, &quality); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan video_features: %w", err)
			}
			pageRows++
			after = pgtype.Text{String: shaText, Valid: true}
			sha, shaOK := shaFromText(shaText)
			pdq, pdqOK := pdqFromBytes(pdqBytes)
			if !shaOK || !pdqOK {
				s.badRows++
				continue
			}
			feature := VideoFeature{
				SHA512:     sha,
				DurationMs: duration,
				ThumbPDQ:   pdq,
			}
			if quality.Valid {
				feature.ThumbQuality = int(quality.Int32)
			}
			features = append(features, feature)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read video_features: %w", err)
		}
		rows.Close()
		if pageRows < s.cfg.ReadPageSize {
			return features, nil
		}
	}
}

func (s *Store) StreamFilesBySHA(
	ctx context.Context,
	visit func([64]byte, FileRef) error,
) error {
	var (
		afterSHA pgtype.Text
		afterID  int64
	)
	for {
		rows, err := s.conn.Query(ctx, `
			SELECT sha512,id,machine_id,disk_no,path,size
			FROM files
			WHERE sha512 IS NOT NULL
			  AND ($1::text IS NULL OR (sha512,id) > ($1::text,$2))
			ORDER BY sha512,id
			LIMIT $3`,
			afterSHA, afterID, s.cfg.ReadPageSize)
		if err != nil {
			return fmt.Errorf("query files by SHA-512: %w", err)
		}

		pageRows := 0
		for rows.Next() {
			var (
				shaText string
				file    FileRef
			)
			if err := rows.Scan(
				&shaText,
				&file.ID,
				&file.MachineID,
				&file.DiskNo,
				&file.Path,
				&file.Size,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan files by SHA-512: %w", err)
			}
			pageRows++
			afterSHA = pgtype.Text{String: shaText, Valid: true}
			afterID = file.ID
			sha, ok := shaFromText(shaText)
			if !ok {
				rows.Close()
				return fmt.Errorf(
					"files.sha512 %q is not canonical lowercase SHA-512",
					shaText,
				)
			}
			if err := visit(sha, file); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read files by SHA-512: %w", err)
		}
		rows.Close()
		if pageRows < s.cfg.ReadPageSize {
			return nil
		}
	}
}

func (s *Store) resolveFiles(
	ctx context.Context,
	tx pgx.Tx,
	shaSet map[[64]byte]struct{},
) (map[[64]byte][]FileRef, error) {
	filesBySHA := make(map[[64]byte][]FileRef, len(shaSet))
	shas := make([]string, 0, len(shaSet))
	for sha := range shaSet {
		shas = append(shas, fmt.Sprintf("%x", sha[:]))
	}
	sort.Strings(shas)

	for start := 0; start < len(shas); start += s.cfg.SHAResolveChunk {
		end := min(start+s.cfg.SHAResolveChunk, len(shas))
		rows, err := tx.Query(ctx, qFilesBySHASet, shas[start:end])
		if err != nil {
			return nil, fmt.Errorf("resolve files: %w", err)
		}
		for rows.Next() {
			var (
				file    FileRef
				shaText string
			)
			if err := rows.Scan(
				&file.ID,
				&shaText,
				&file.MachineID,
				&file.DiskNo,
				&file.Path,
				&file.Size,
			); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan resolved files: %w", err)
			}
			sha, ok := shaFromText(shaText)
			if !ok {
				rows.Close()
				return nil, fmt.Errorf(
					"resolved files.sha512 %q is not canonical lowercase SHA-512",
					shaText,
				)
			}
			filesBySHA[sha] = append(filesBySHA[sha], file)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read resolved files: %w", err)
		}
		rows.Close()
	}
	return filesBySHA, nil
}

func (s *Store) ReplaceResults(
	ctx context.Context,
	exact []ExactGroup,
	pairs []CandidatePair,
) (groupsWritten, membersWritten, skipped int, err error) {
	tx, err := s.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin result replacement: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if _, err = tx.Exec(ctx, qDeleteMembersM3, M3Kinds); err != nil {
		return 0, 0, 0, fmt.Errorf("delete M3 members: %w", err)
	}
	if _, err = tx.Exec(ctx, qDeleteGroupsM3, M3Kinds); err != nil {
		return 0, 0, 0, fmt.Errorf("delete M3 groups: %w", err)
	}

	shaSet := make(map[[64]byte]struct{}, len(pairs)*2)
	for _, pair := range pairs {
		shaSet[pair.ShaA] = struct{}{}
		shaSet[pair.ShaB] = struct{}{}
	}
	filesBySHA, err := s.resolveFiles(ctx, tx, shaSet)
	if err != nil {
		return 0, 0, 0, err
	}

	type memberRow struct {
		fileID int64
		score  string
	}
	type groupRow struct {
		kind           string
		representative int64
		members        []memberRow
	}
	groups := make([]groupRow, 0, len(exact)+len(pairs))
	for _, exactGroup := range exact {
		if len(exactGroup.Members) == 0 {
			continue
		}
		group := groupRow{
			kind:           KindExact,
			representative: exactGroup.Members[0].ID,
			members:        make([]memberRow, 0, len(exactGroup.Members)),
		}
		for _, file := range exactGroup.Members {
			if file.ID < group.representative {
				group.representative = file.ID
			}
			group.members = append(group.members, memberRow{
				fileID: file.ID,
				score:  `{"basis":"sha512"}`,
			})
		}
		groups = append(groups, group)
	}
	for _, pair := range pairs {
		sideA := filesBySHA[pair.ShaA]
		sideB := filesBySHA[pair.ShaB]
		if len(sideA) == 0 || len(sideB) == 0 {
			skipped++
			continue
		}
		group := groupRow{
			kind:           pair.Kind,
			representative: sideA[0].ID,
			members:        make([]memberRow, 0, len(sideA)+len(sideB)),
		}
		for _, file := range sideA {
			if file.ID < group.representative {
				group.representative = file.ID
			}
			group.members = append(group.members, memberRow{
				fileID: file.ID,
				score:  string(pair.scoreJSON(true)),
			})
		}
		for _, file := range sideB {
			group.members = append(group.members, memberRow{
				fileID: file.ID,
				score:  string(pair.scoreJSON(false)),
			})
		}
		groups = append(groups, group)
	}

	allMembers := make([][]any, 0)
	for start := 0; start < len(groups); start += s.cfg.GroupInsertBatch {
		end := min(start+s.cfg.GroupInsertBatch, len(groups))
		batch := &pgx.Batch{}
		for _, group := range groups[start:end] {
			batch.Queue(
				qInsertGroup,
				group.kind,
				group.representative,
				len(group.members),
			)
		}
		results := tx.SendBatch(ctx, batch)
		for _, group := range groups[start:end] {
			var groupID int64
			if err = results.QueryRow().Scan(&groupID); err != nil {
				_ = results.Close()
				return 0, 0, 0, fmt.Errorf("insert M3 group: %w", err)
			}
			for _, member := range group.members {
				allMembers = append(allMembers, []any{
					groupID,
					member.fileID,
					member.score,
				})
			}
			groupsWritten++
		}
		if err = results.Close(); err != nil {
			return 0, 0, 0, fmt.Errorf("close M3 group batch: %w", err)
		}
	}

	if len(allMembers) > 0 {
		if _, err = tx.CopyFrom(
			ctx,
			pgx.Identifier{"dup_members"},
			[]string{"group_id", "file_id", "score_json"},
			pgx.CopyFromRows(allMembers),
		); err != nil {
			return 0, 0, 0, fmt.Errorf("copy M3 members: %w", err)
		}
		membersWritten = len(allMembers)
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		if errors.Is(commitErr, pgx.ErrTxCommitRollback) {
			return 0, 0, 0, fmt.Errorf(
				"commit result replacement rolled back: %w",
				commitErr,
			)
		}
		return 0, 0, 0, errors.Join(
			ErrCommitOutcomeUnknown,
			fmt.Errorf("commit result replacement: %w", commitErr),
		)
	}
	return groupsWritten, membersWritten, skipped, nil
}
