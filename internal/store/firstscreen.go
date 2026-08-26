package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"dedup/internal/firstscreen"
)

const localFeatureSHABatchSize = 500

func (d *DB) StreamActiveFiles(ctx context.Context, machineID string, visit func(firstscreen.File) error) error {
	if machineID == "" {
		return fmt.Errorf("store: stream active candidates: empty machine ID")
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT f.id,f.machine_id,f.disk_no,f.path,f.size,f.sha512
		FROM files f
		WHERE f.machine_id=?1 AND f.status!='deleted' AND f.sha512 IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM local_delete_items d
		    WHERE d.machine_id=f.machine_id AND d.file_id=f.id
		      AND d.result IN ('pending','uncertain'))
		ORDER BY f.sha512,f.id`, machineID)
	if err != nil {
		return fmt.Errorf("store: query active candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var file firstscreen.File
		var shaText string
		if err := rows.Scan(&file.ID, &file.MachineID, &file.DiskNo, &file.Path, &file.Size, &shaText); err != nil {
			return fmt.Errorf("store: scan active candidate: %w", err)
		}
		sha, ok := firstscreenSHA(shaText)
		if !ok {
			return fmt.Errorf("store: active candidate has invalid SHA-512")
		}
		file.SHA512 = sha
		if err := visit(file); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (d *DB) LoadImageFeatures(ctx context.Context, shas []string) (map[string]firstscreen.ImageFeature, error) {
	result := make(map[string]firstscreen.ImageFeature)
	for start := 0; start < len(shas); start += localFeatureSHABatchSize {
		end := min(start+localFeatureSHABatchSize, len(shas))
		rows, err := d.db.QueryContext(ctx, `
			SELECT sha512,width,height,pdq256,pdq_quality FROM image_features
			WHERE sha512 IN (`+sqlPlaceholders(end-start)+`)`, stringsToAny(shas[start:end])...)
		if err != nil {
			return nil, fmt.Errorf("store: query local image features: %w", err)
		}
		for rows.Next() {
			var text string
			var width, height, quality int
			var pdqBytes []byte
			if err := rows.Scan(&text, &width, &height, &pdqBytes, &quality); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan local image feature: %w", err)
			}
			sha, shaOK := firstscreenSHA(text)
			pdq, pdqOK := firstscreenPDQ(pdqBytes)
			if !shaOK || !pdqOK {
				continue
			}
			result[text] = firstscreen.ImageFeature{SHA512: sha, PDQ: pdq, Quality: quality, Width: width, Height: height}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (d *DB) LoadVideoFeatures(ctx context.Context, shas []string) (map[string]firstscreen.VideoFeature, error) {
	result := make(map[string]firstscreen.VideoFeature)
	for start := 0; start < len(shas); start += localFeatureSHABatchSize {
		end := min(start+localFeatureSHABatchSize, len(shas))
		rows, err := d.db.QueryContext(ctx, `
			SELECT sha512,duration_ms,thumb_pdq256,thumb_quality
			FROM video_features WHERE sha512 IN (`+sqlPlaceholders(end-start)+`)`, stringsToAny(shas[start:end])...)
		if err != nil {
			return nil, fmt.Errorf("store: query local video features: %w", err)
		}
		for rows.Next() {
			var text string
			var duration sql.NullInt64
			var quality sql.NullInt64
			var pdqBytes []byte
			if err := rows.Scan(&text, &duration, &pdqBytes, &quality); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan local video feature: %w", err)
			}
			sha, shaOK := firstscreenSHA(text)
			pdq, pdqOK := firstscreenPDQ(pdqBytes)
			if !shaOK || !pdqOK || !duration.Valid {
				continue
			}
			feature := firstscreen.VideoFeature{SHA512: sha, DurationMs: duration.Int64, ThumbPDQ: pdq}
			if quality.Valid {
				feature.ThumbQuality = int(quality.Int64)
			}
			result[text] = feature
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (d *DB) ReplaceStageOne(ctx context.Context, runID string, result firstscreen.Result) error {
	if runID == "" {
		return fmt.Errorf("store: replace stage one: empty run ID")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var machineID string
	if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM local_analysis_runs WHERE run_id=?1 AND status='building'`, runID).Scan(&machineID); err != nil {
		return fmt.Errorf("store: replace stage one: run is not building: %w", err)
	}
	files, err := activeResultFiles(ctx, tx, machineID, result.Files)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_pair_scores WHERE run_id=?1`, runID); err != nil {
		return fmt.Errorf("store: clear stage one pairs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_dup_members WHERE run_id=?1`, runID); err != nil {
		return fmt.Errorf("store: clear stage one members: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_dup_groups WHERE run_id=?1`, runID); err != nil {
		return fmt.Errorf("store: clear stage one groups: %w", err)
	}
	bySHA := filesBySHA(files)
	for _, group := range result.ExactGroups {
		members := exactActiveMembers(group, files)
		if len(members) >= 2 {
			if err := insertLocalCandidateGroup(ctx, tx, machineID, runID, "exact", "duplicate", members); err != nil {
				return err
			}
		}
	}
	for _, pair := range result.CandidatePairs {
		left, right := bySHA[pair.ShaA], bySHA[pair.ShaB]
		if len(left) == 0 || len(right) == 0 {
			continue
		}
		category := "image"
		if pair.Kind == firstscreen.KindVideoCandidate {
			category = "video"
		}
		members := append(append([]firstscreen.File(nil), left...), right...)
		if err := insertLocalCandidateGroup(ctx, tx, machineID, runID, category, "uncertain", members); err != nil {
			return err
		}
		if err := insertLocalPairScore(ctx, tx, runID, pair, left[0], right[0]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func activeResultFiles(ctx context.Context, tx *sql.Tx, machineID string, files []firstscreen.File) ([]firstscreen.File, error) {
	result := make([]firstscreen.File, 0, len(files))
	seen := make(map[int64]struct{}, len(files))
	for _, file := range files {
		if file.ID == 0 || file.MachineID != machineID {
			return nil, fmt.Errorf("store: replace stage one: file identity mismatch")
		}
		if _, exists := seen[file.ID]; exists {
			return nil, fmt.Errorf("store: replace stage one: duplicate file identity")
		}
		seen[file.ID] = struct{}{}
		var storedSHA string
		if err := tx.QueryRowContext(ctx, `SELECT sha512 FROM files WHERE id=?1 AND machine_id=?2 AND status!='deleted'`, file.ID, machineID).Scan(&storedSHA); err != nil {
			return nil, fmt.Errorf("store: replace stage one: active file identity mismatch: %w", err)
		}
		sha, ok := firstscreenSHA(storedSHA)
		if !ok || sha != file.SHA512 {
			return nil, fmt.Errorf("store: replace stage one: active file SHA mismatch")
		}
		result = append(result, file)
	}
	return result, nil
}

func exactActiveMembers(group firstscreen.ExactGroup, files []firstscreen.File) []firstscreen.File {
	ids := make(map[int64]struct{}, len(group.Members))
	for _, member := range group.Members {
		ids[member.ID] = struct{}{}
	}
	result := make([]firstscreen.File, 0, len(group.Members))
	for _, file := range files {
		if file.SHA512 == group.SHA512 {
			if _, ok := ids[file.ID]; ok {
				result = append(result, file)
			}
		}
	}
	return result
}

func filesBySHA(files []firstscreen.File) map[[64]byte][]firstscreen.File {
	result := make(map[[64]byte][]firstscreen.File)
	for _, file := range files {
		result[file.SHA512] = append(result[file.SHA512], file)
	}
	for _, group := range result {
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	}
	return result
}

func insertLocalCandidateGroup(ctx context.Context, tx *sql.Tx, machineID, runID, category, verdict string, members []firstscreen.File) error {
	groupID, err := newLocalID()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at)
		SELECT ?2,?3,r.run_id,r.generation,?4,?5,?6 FROM local_analysis_runs r WHERE r.run_id=?1`, runID, groupID, machineID, category, verdict, now); err != nil {
		return fmt.Errorf("store: insert stage one group: %w", err)
	}
	for _, member := range members {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at)
			SELECT ?2,?3,r.run_id,r.generation,?4,?5,?6 FROM local_analysis_runs r WHERE r.run_id=?1`, runID, groupID, machineID, member.ID, hex.EncodeToString(member.SHA512[:]), now); err != nil {
			return fmt.Errorf("store: insert stage one member: %w", err)
		}
	}
	return nil
}

func insertLocalPairScore(ctx context.Context, tx *sql.Tx, runID string, pair firstscreen.CandidatePair, left, right firstscreen.File) error {
	pairKey := pair.Kind + ":" + hex.EncodeToString(pair.ShaA[:]) + ":" + hex.EncodeToString(pair.ShaB[:])
	document := map[string]any{"kind": pair.Kind, "verdict": "undecided", "hamming": pair.Hamming, "quality_a": pair.QualityA, "quality_b": pair.QualityB}
	if pair.Kind == firstscreen.KindVideoCandidate {
		document["duration_diff_ms"] = pair.DurationDiffMs
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO local_pair_scores(machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,stage1_json,final_verdict,created_at,updated_at)
		SELECT r.machine_id,r.run_id,r.generation,?2,?3,?4,?5,?6,?7,'undecided',?8,?8
		FROM local_analysis_runs r WHERE r.run_id=?1`, runID, pairKey, left.ID, right.ID, hex.EncodeToString(pair.ShaA[:]), hex.EncodeToString(pair.ShaB[:]), string(payload), now)
	return err
}

func firstscreenSHA(text string) ([64]byte, bool) {
	var result [64]byte
	if len(text) != 128 || strings.ToLower(text) != text {
		return result, false
	}
	decoded, err := hex.DecodeString(text)
	if err != nil || len(decoded) != len(result) {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

func firstscreenPDQ(bytes []byte) ([4]uint64, bool) {
	var result [4]uint64
	if len(bytes) != 32 {
		return result, false
	}
	for index := range result {
		result[index] = uint64(bytes[index*8])<<56 | uint64(bytes[index*8+1])<<48 | uint64(bytes[index*8+2])<<40 | uint64(bytes[index*8+3])<<32 | uint64(bytes[index*8+4])<<24 | uint64(bytes[index*8+5])<<16 | uint64(bytes[index*8+6])<<8 | uint64(bytes[index*8+7])
	}
	return result, true
}

func sqlPlaceholders(length int) string { return strings.TrimRight(strings.Repeat("?,", length), ",") }

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

var _ firstscreen.CandidateSource = (*DB)(nil)
var _ firstscreen.CandidateSink = (*DB)(nil)
