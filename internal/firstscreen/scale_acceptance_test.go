//go:build m3scale

package firstscreen

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	scaleImageRandom   = 960_000
	scaleImageClusters = 10_000
	scaleImageClusterN = 4
	scaleImageRows     = scaleImageRandom + scaleImageClusters*scaleImageClusterN

	scaleVideoRandom   = 190_000
	scaleVideoClusters = 2_500
	scaleVideoClusterN = 4
	scaleVideoRows     = scaleVideoRandom + scaleVideoClusters*scaleVideoClusterN
	scaleVideoUnits    = scaleVideoRandom + scaleVideoClusters

	scaleExactGroups = 50_000
	scaleFileRows    = 1_350_000
	scaleCopyChunk   = 50_000

	scaleDomainImage byte = 1
	scaleDomainVideo byte = 2
	scaleDomainExact byte = 3
)

func scaleExactCopies(group int) int {
	copies := 2 + group%3
	if group == scaleExactGroups-1 {
		copies++
	}
	return copies
}

func scaleSHA(domain byte, ordinal uint64) [64]byte {
	var sha [64]byte
	sha[0] = domain
	binary.BigEndian.PutUint64(sha[len(sha)-8:], ordinal)
	return sha
}

func scaleSHAText(sha [64]byte) string {
	return hex.EncodeToString(sha[:])
}

func scaleImageRandomFeature(ordinal int) ImageFeature {
	value := uint64(ordinal + 1)
	quality := 50 + ordinal%50
	if ordinal%100 == 0 {
		quality = 30
	}
	return ImageFeature{
		SHA512: scaleSHA(scaleDomainImage, uint64(ordinal)),
		PDQ: [4]uint64{
			0x1000_0000_0000_0000 | value,
			0x2000_0000_0000_0000 | value,
			0x3000_0000_0000_0000 | value,
			0x4000_0000_0000_0000 | value,
		},
		Quality: quality,
		Width:   1920,
		Height:  1080,
	}
}

type scaleRowGenerator interface {
	NextRow([]any) ([]any, bool, error)
}

type scaleChunkSource struct {
	generator scaleRowGenerator
	remaining int
	values    []any
	err       error
}

func (s *scaleChunkSource) Next() bool {
	if s.err != nil || s.remaining == 0 {
		return false
	}
	var ok bool
	s.values, ok, s.err = s.generator.NextRow(s.values)
	if s.err != nil || !ok {
		if s.err == nil {
			s.err = errors.New("scale generator ended before declared total")
		}
		return false
	}
	s.remaining--
	return true
}

func (s *scaleChunkSource) Values() ([]any, error) {
	return s.values, s.err
}

func (s *scaleChunkSource) Err() error {
	return s.err
}

type scaleImageRowGenerator struct {
	index    int
	pdqBytes [32]byte
}

func (g *scaleImageRowGenerator) NextRow(values []any) ([]any, bool, error) {
	if g.index >= scaleImageRows {
		return nil, false, nil
	}
	var feature ImageFeature
	if g.index < scaleImageRandom {
		feature = scaleImageRandomFeature(g.index)
	} else {
		offset := g.index - scaleImageRandom
		feature = scaleImageClusterFeature(
			offset/scaleImageClusterN,
			offset%scaleImageClusterN,
		)
	}
	scaleEncodePDQ(g.pdqBytes[:], feature.PDQ)
	if cap(values) < 5 {
		values = make([]any, 5)
	} else {
		values = values[:5]
	}
	values[0] = scaleSHAText(feature.SHA512)
	values[1] = feature.Width
	values[2] = feature.Height
	values[3] = g.pdqBytes[:]
	values[4] = feature.Quality
	g.index++
	return values, true, nil
}

type scaleVideoRowGenerator struct {
	features *scaleVideoGenerator
	pdqBytes [32]byte
}

func (g *scaleVideoRowGenerator) NextRow(values []any) ([]any, bool, error) {
	feature, ok, err := g.features.NextFeature()
	if err != nil || !ok {
		return nil, ok, err
	}
	scaleEncodePDQ(g.pdqBytes[:], feature.ThumbPDQ)
	if cap(values) < 4 {
		values = make([]any, 4)
	} else {
		values = values[:4]
	}
	values[0] = scaleSHAText(feature.SHA512)
	values[1] = feature.DurationMs
	values[2] = g.pdqBytes[:]
	values[3] = feature.ThumbQuality
	return values, true, nil
}

type scaleFileRowGenerator struct {
	index      int
	exactGroup int
	exactCopy  int
}

func (g *scaleFileRowGenerator) NextRow(values []any) ([]any, bool, error) {
	if g.index >= scaleFileRows {
		return nil, false, nil
	}
	if cap(values) < 5 {
		values = make([]any, 5)
	} else {
		values = values[:5]
	}
	switch {
	case g.index < scaleImageRows:
		ordinal := g.index
		values[0] = "m1"
		values[1] = ordinal % 3
		values[2] = fmt.Sprintf("D:/m3scale/image/%07d.jpg", ordinal)
		values[3] = int64(1_000_000 + ordinal)
		values[4] = scaleSHAText(scaleSHA(scaleDomainImage, uint64(ordinal)))
	case g.index < scaleImageRows+scaleVideoRows:
		ordinal := g.index - scaleImageRows
		values[0] = "m1"
		values[1] = ordinal % 3
		values[2] = fmt.Sprintf("D:/m3scale/video/%07d.mp4", ordinal)
		values[3] = int64(50_000_000 + ordinal)
		values[4] = scaleSHAText(scaleSHA(scaleDomainVideo, uint64(ordinal)))
	default:
		if g.exactGroup >= scaleExactGroups {
			return nil, false, errors.New("exact generator exceeded 50000 groups")
		}
		values[0] = fmt.Sprintf("m%d", g.exactCopy%2+1)
		values[1] = g.exactCopy % 3
		values[2] = fmt.Sprintf(
			"D:/m3scale/exact/%05d_%d.bin",
			g.exactGroup,
			g.exactCopy,
		)
		values[3] = int64(5_000_000)
		values[4] = scaleSHAText(
			scaleSHA(scaleDomainExact, uint64(g.exactGroup)),
		)
		g.exactCopy++
		if g.exactCopy == scaleExactCopies(g.exactGroup) {
			g.exactGroup++
			g.exactCopy = 0
		}
	}
	g.index++
	return values, true, nil
}

func scaleEncodePDQ(dst []byte, pdq [4]uint64) {
	for band := range pdq {
		binary.BigEndian.PutUint64(dst[band*8:(band+1)*8], pdq[band])
	}
}

type scalePlanEvidence struct {
	RootNodeType     string   `json:"root_node_type"`
	IndexNames       []string `json:"index_names"`
	ActualRows       int64    `json:"actual_rows"`
	PlanningMS       float64  `json:"planning_ms"`
	ExecutionMS      float64  `json:"execution_ms"`
	SharedHitBlocks  int64    `json:"shared_hit_blocks"`
	SharedReadBlocks int64    `json:"shared_read_blocks"`
	Actual           bool     `json:"actual"`
}

type scaleRunEvidence struct {
	Ordinal      int              `json:"ordinal"`
	Counts       map[string]int   `json:"counts"`
	StageMS      map[string]int64 `json:"stage_ms"`
	StageKeys    []string         `json:"stage_keys"`
	TotalMS      int64            `json:"total_ms"`
	PeakHeapByte uint64           `json:"peak_heap_bytes"`
}

type scaleAcceptanceMarker struct {
	RunID               string                       `json:"run_id"`
	Schema              string                       `json:"schema"`
	Seed                int                          `json:"seed"`
	Seeded              bool                         `json:"seeded"`
	Reused              bool                         `json:"reused"`
	PostgreSQLVersion   string                       `json:"postgresql_version"`
	PublicUnchanged     bool                         `json:"public_unchanged"`
	CentralSQLRuns      int                          `json:"central_sql_runs"`
	CopyChunkRows       int                          `json:"copy_chunk_rows"`
	SeedDurationMS      int64                        `json:"seed_duration_ms"`
	SeedChunks          map[string]int               `json:"seed_chunks"`
	PhysicalRows        map[string]int64             `json:"physical_rows"`
	Plans               map[string]scalePlanEvidence `json:"plans"`
	Runs                []scaleRunEvidence           `json:"runs"`
	DBTotals            map[string]int64             `json:"db_totals"`
	SemanticIdempotent  bool                         `json:"semantic_idempotent"`
	PerformancePass     bool                         `json:"performance_pass"`
	PhysicalVerified    bool                         `json:"physical_verified"`
	SchemaPreserved     bool                         `json:"schema_preserved"`
	CleanupPerformed    bool                         `json:"cleanup_performed"`
	CleanupResidual     int64                        `json:"cleanup_residual"`
	SecondWindowsStatus string                       `json:"second_windows_status"`
}

type scaleCleanupMarker struct {
	RunID           string `json:"run_id"`
	Schema          string `json:"schema"`
	CleanupResidual int64  `json:"cleanup_residual"`
}

func newScaleCleanupMarker(
	runID string,
	schema string,
	residual int64,
) (scaleCleanupMarker, error) {
	marker := scaleCleanupMarker{
		RunID:           runID,
		Schema:          schema,
		CleanupResidual: residual,
	}
	if residual != 0 {
		return marker, fmt.Errorf(
			"scale cleanup residual=%d, want 0",
			residual,
		)
	}
	return marker, nil
}

type scalePublicWindow struct {
	baseline     []string
	finalChecked bool
	unchanged    bool
}

func newScalePublicWindow(baseline []string) *scalePublicWindow {
	return &scalePublicWindow{
		baseline: append([]string(nil), baseline...),
	}
}

func (w *scalePublicWindow) CheckIntermediate(current []string) error {
	if !reflect.DeepEqual(current, w.baseline) {
		return errors.New("public catalog changed before final lifecycle snapshot")
	}
	return nil
}

func (w *scalePublicWindow) CheckFinal(current []string) {
	w.finalChecked = true
	w.unchanged = reflect.DeepEqual(current, w.baseline)
}

func (w *scalePublicWindow) Unchanged() bool {
	return w.finalChecked && w.unchanged
}

type scaleHeapSampler struct {
	peak atomic.Uint64
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func newScaleHeapSampler() *scaleHeapSampler {
	sampler := &scaleHeapSampler{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	sampler.sample()
	go func() {
		defer close(sampler.done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampler.stop:
				return
			case <-ticker.C:
				sampler.sample()
			}
		}
	}()
	return sampler
}

func (s *scaleHeapSampler) sample() {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	for {
		current := s.peak.Load()
		if memory.HeapInuse <= current ||
			s.peak.CompareAndSwap(current, memory.HeapInuse) {
			return
		}
	}
}

func (s *scaleHeapSampler) Stop() uint64 {
	s.once.Do(func() {
		s.sample()
		close(s.stop)
		<-s.done
	})
	return s.peak.Load()
}

func scaleImageClusterFeature(cluster, member int) ImageFeature {
	clusterKey := uint64(cluster + 1)
	memberKey := clusterKey<<2 | uint64(member)
	return ImageFeature{
		SHA512: scaleSHA(
			scaleDomainImage,
			uint64(scaleImageRandom+cluster*scaleImageClusterN+member),
		),
		PDQ: [4]uint64{
			0x8000_0000_0000_0000 | clusterKey,
			0x9000_0000_0000_0000 | memberKey,
			0xa000_0000_0000_0000 | memberKey,
			0xb000_0000_0000_0000 | memberKey,
		},
		Quality: 60 + (cluster+member)%36,
		Width:   1920,
		Height:  1080,
	}
}

type scaleVideoGenerator struct {
	blocks      int
	row         int
	active      []VideoFeature
	maxActive   int
	clusterBase [4]uint64
}

func newScaleVideoGenerator(blocks int) *scaleVideoGenerator {
	return &scaleVideoGenerator{blocks: blocks}
}

func (g *scaleVideoGenerator) MaxActive() int {
	return g.maxActive
}

func (g *scaleVideoGenerator) NextFeature() (VideoFeature, bool, error) {
	if g.row >= g.blocks*80 {
		return VideoFeature{}, false, nil
	}
	block := g.row / 80
	position := g.row % 80
	unit := block*77 + min(position, 76)
	duration := int64(unit) * 3_600_000 / scaleVideoUnits
	g.prune(duration)

	var pdq [4]uint64
	if position < 76 {
		var err error
		pdq, err = g.safePDQ(g.row, 31)
		if err != nil {
			return VideoFeature{}, false, err
		}
	} else {
		member := position - 76
		if member == 0 {
			var err error
			g.clusterBase, err = g.safePDQ(g.row, 34)
			if err != nil {
				return VideoFeature{}, false, err
			}
		}
		pdq = g.clusterBase
		pdq[0] ^= (uint64(1) << member) - 1
	}

	feature := VideoFeature{
		SHA512:       scaleSHA(scaleDomainVideo, uint64(g.row)),
		DurationMs:   duration,
		ThumbPDQ:     pdq,
		ThumbQuality: 50 + g.row%50,
	}
	g.active = append(g.active, feature)
	if len(g.active) > g.maxActive {
		g.maxActive = len(g.active)
	}
	g.row++
	return feature, true, nil
}

func (g *scaleVideoGenerator) prune(duration int64) {
	first := 0
	for first < len(g.active) &&
		duration-g.active[first].DurationMs > 2_000 {
		first++
	}
	if first > 0 {
		copy(g.active, g.active[first:])
		g.active = g.active[:len(g.active)-first]
	}
}

func (g *scaleVideoGenerator) safePDQ(row, minDistance int) ([4]uint64, error) {
	var input [24]byte
	copy(input[:8], "m3seed1")
	binary.BigEndian.PutUint64(input[8:16], uint64(row))
	for nonce := uint64(0); nonce < 10_000; nonce++ {
		binary.BigEndian.PutUint64(input[16:24], nonce)
		digest := sha256.Sum256(input[:])
		var candidate [4]uint64
		for band := range candidate {
			candidate[band] = binary.BigEndian.Uint64(
				digest[band*8 : (band+1)*8],
			)
		}
		safe := true
		for _, prior := range g.active {
			if hamming256(candidate, prior.ThumbPDQ) <= minDistance {
				safe = false
				break
			}
		}
		if safe {
			return candidate, nil
		}
	}
	return [4]uint64{}, fmt.Errorf(
		"no video PDQ outside distance %d at row %d",
		minDistance,
		row,
	)
}

// TestScaleGeneratorArithmetic catches a change that would silently seed the
// documented physical totals while producing only 149,999 exact members.
func TestScaleGeneratorArithmetic(t *testing.T) {
	exactMembers := 0
	for group := 0; group < scaleExactGroups; group++ {
		exactMembers += scaleExactCopies(group)
	}
	if exactMembers != 150_000 {
		t.Fatalf("exact members = %d, want 150000", exactMembers)
	}
	if scaleImageRows != 1_000_000 {
		t.Fatalf("image rows = %d, want 1000000", scaleImageRows)
	}
	if scaleVideoRows != 200_000 {
		t.Fatalf("video rows = %d, want 200000", scaleVideoRows)
	}
	if scaleFileRows != 1_350_000 {
		t.Fatalf("file rows = %d, want 1350000", scaleFileRows)
	}
}

// TestScaleGeneratorCanonicalSHADomains catches cross-domain identity reuse:
// image, video, and exact rows must never accidentally form exact groups.
func TestScaleGeneratorCanonicalSHADomains(t *testing.T) {
	image := scaleSHA(scaleDomainImage, 0)
	video := scaleSHA(scaleDomainVideo, 0)
	exact := scaleSHA(scaleDomainExact, 0)
	if image == video || image == exact || video == exact {
		t.Fatalf("scale SHA domains overlap: image=%x video=%x exact=%x",
			image, video, exact)
	}
	for _, sha := range [][64]byte{image, video, exact} {
		text := scaleSHAText(sha)
		if len(text) != 128 {
			t.Fatalf("canonical SHA length = %d, want 128", len(text))
		}
		parsed, ok := shaFromText(text)
		if !ok || parsed != sha {
			t.Fatalf("canonical SHA did not round-trip: %q", text)
		}
	}
}

// TestScaleImageGeneratorHasOnlyClusterPairs catches any band-key overlap
// between random rows or different clusters.
func TestScaleImageGeneratorHasOnlyClusterPairs(t *testing.T) {
	features := make([]ImageFeature, 0, 112)
	for ordinal := 0; ordinal < 100; ordinal++ {
		features = append(features, scaleImageRandomFeature(ordinal))
	}
	for cluster := 0; cluster < 3; cluster++ {
		for member := 0; member < scaleImageClusterN; member++ {
			features = append(features, scaleImageClusterFeature(cluster, member))
		}
	}
	pairs := screenImages(features, 31, 0.10, 50)
	if len(pairs) != 18 {
		t.Fatalf("image pairs = %d, want 18", len(pairs))
	}
}

// TestScaleVideoGeneratorHasOnlyClusterPairs catches accidental close PDQs in
// the 2-second duration window. The generator itself guards each non-cluster
// row against its bounded active window.
func TestScaleVideoGeneratorHasOnlyClusterPairs(t *testing.T) {
	generator := newScaleVideoGenerator(3)
	features := make([]VideoFeature, 0, 3*80)
	for {
		feature, ok, err := generator.NextFeature()
		if err != nil {
			t.Fatalf("generate video feature: %v", err)
		}
		if !ok {
			break
		}
		features = append(features, feature)
	}
	if len(features) != 240 {
		t.Fatalf("video features = %d, want 240", len(features))
	}
	if generator.MaxActive() > 500 {
		t.Fatalf("video active window = %d, want <= 500", generator.MaxActive())
	}
	pairs := screenVideos(features, 2_000, 31)
	if len(pairs) != 18 {
		t.Fatalf("video pairs = %d, want 18", len(pairs))
	}
}

func TestScaleCleanupMarkerRejectsNonzeroResidual(t *testing.T) {
	marker, err := newScaleCleanupMarker(
		"cleanup-marker-test",
		"m3_scale_cleanup_marker_test",
		0,
	)
	if err != nil {
		t.Fatalf("zero residual cleanup marker: %v", err)
	}
	if marker.CleanupResidual != 0 {
		t.Fatalf("cleanup residual = %d, want 0", marker.CleanupResidual)
	}
	if _, err := newScaleCleanupMarker(
		"cleanup-marker-test",
		"m3_scale_cleanup_marker_test",
		1,
	); err == nil {
		t.Fatal("non-zero cleanup residual was accepted")
	}
}

func TestScalePublicWindowRequiresFinalSnapshot(t *testing.T) {
	window := newScalePublicWindow([]string{"relation\tfiles\tr"})
	if window.Unchanged() {
		t.Fatal("public window defaulted to unchanged before final snapshot")
	}
	if err := window.CheckIntermediate(
		[]string{"relation\tfiles\tr"},
	); err != nil {
		t.Fatalf("matching intermediate snapshot: %v", err)
	}
	if window.Unchanged() {
		t.Fatal("intermediate snapshot incorrectly completed public window")
	}
	window.CheckFinal([]string{"relation\tfiles\tr"})
	if !window.Unchanged() {
		t.Fatal("matching final snapshot did not complete public window")
	}

	changed := newScalePublicWindow([]string{"relation\tfiles\tr"})
	changed.CheckFinal([]string{
		"relation\tfiles\tr",
		"sequence\tunexpected\tS",
	})
	if changed.Unchanged() {
		t.Fatal("changed final snapshot was accepted")
	}
}

func TestAcceptanceM3(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("FS_PG_DSN"))
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run M3 scale acceptance")
	}
	runID := strings.TrimSpace(os.Getenv("M3_VERIFY_RUN_ID"))
	if runID == "" {
		t.Fatal("M3_VERIFY_RUN_ID must be explicit")
	}
	schema := strings.TrimSpace(os.Getenv("FS_M3_SCHEMA"))
	if err := scaleValidateSchema(runID, schema); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := conn.Close(closeCtx); err != nil {
			t.Errorf("close PostgreSQL: %v", err)
		}
	})

	if os.Getenv("FS_M3_CLEANUP_ONLY") == "1" {
		residual := scaleDropSchema(t, conn, runID, schema)
		marker, err := newScaleCleanupMarker(runID, schema, residual)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(marker)
		if err != nil {
			t.Fatalf("marshal scale cleanup marker: %v", err)
		}
		t.Logf("M3_SCALE_CLEANUP %s", raw)
		return
	}

	seedText := strings.TrimSpace(os.Getenv("FS_M3_SEED"))
	if seedText != "0" && seedText != "1" {
		t.Fatal("FS_M3_SEED must be exactly 0 or 1")
	}
	seeded := seedText == "1"
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	cleanupNeeded := false
	preserveSchema := false
	t.Cleanup(func() {
		if cleanupNeeded && (!preserveSchema || t.Failed()) {
			_ = scaleDropSchema(t, conn, runID, schema)
		}
	})

	version := scalePostgreSQLVersion(t, conn)
	var publicWindow *scalePublicWindow
	centralRuns := 0
	seedDuration := int64(0)
	seedChunks := map[string]int{
		"image_features": 0,
		"video_features": 0,
		"files":          0,
	}

	if seeded {
		publicWindow = newScalePublicWindow(
			task4PublicSchemaSnapshot(t, conn),
		)
		if scaleSchemaExists(t, conn, schema) {
			t.Fatalf("scale seed schema already exists: %s", schema)
		}
		if _, err := conn.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
			t.Fatalf("create scale schema: %v", err)
		}
		cleanupNeeded = true
		if _, err := conn.Exec(ctx, `SET search_path TO `+quotedSchema); err != nil {
			t.Fatalf("set scale search_path: %v", err)
		}
		centralSQL := smallAcceptanceCentralSQL(t)
		for run := 1; run <= 2; run++ {
			if _, err := conn.Exec(ctx, centralSQL); err != nil {
				t.Fatalf("apply central.sql run %d: %v", run, err)
			}
			centralRuns++
		}
		if err := publicWindow.CheckIntermediate(
			task4PublicSchemaSnapshot(t, conn),
		); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		seedChunks = scaleSeedDatabase(t, conn)
		seedDuration = time.Since(started).Milliseconds()
	} else {
		publicWindow = newScalePublicWindow(
			task4PublicSchemaSnapshot(t, conn),
		)
		if !scaleSchemaExists(t, conn, schema) {
			t.Fatalf("scale reuse schema does not exist: %s", schema)
		}
		cleanupNeeded = true
		if _, err := conn.Exec(ctx, `SET search_path TO `+quotedSchema); err != nil {
			t.Fatalf("set reused scale search_path: %v", err)
		}
	}

	physical := scalePhysicalRows(t, conn)
	scaleAssertPhysicalRows(t, physical)
	plans := map[string]scalePlanEvidence{
		"files": scaleExplain(t, conn, `
			SELECT sha512,id,machine_id,disk_no,path,size
			FROM files
			WHERE sha512 IS NOT NULL
			ORDER BY sha512,id
			LIMIT 50000`),
		"image_features": scaleExplain(t, conn, `
			SELECT sha512,width,height,pdq256,pdq_quality
			FROM image_features
			WHERE pdq256 IS NOT NULL AND pdq_quality >= 50
			ORDER BY sha512
			LIMIT 50000`),
		"video_features": scaleExplain(t, conn, `
			SELECT sha512,duration_ms,thumb_pdq256,thumb_quality
			FROM video_features
			WHERE thumb_pdq256 IS NOT NULL AND duration_ms IS NOT NULL
			ORDER BY sha512
			LIMIT 50000`),
	}

	var beforeReuse []string
	if !seeded {
		beforeReuse = scaleSemanticSignature(t, conn)
	}
	runCount := 1
	if seeded {
		runCount = 2
	}
	runs := make([]scaleRunEvidence, 0, runCount)
	violations := make([]string, 0)
	var firstSignature []string
	for ordinal := 1; ordinal <= runCount; ordinal++ {
		run, runViolations := scaleRunAnalyzer(t, conn, ordinal)
		runs = append(runs, run)
		violations = append(violations, runViolations...)
		signature := scaleSemanticSignature(t, conn)
		if ordinal == 1 {
			firstSignature = signature
		} else if !reflect.DeepEqual(signature, firstSignature) {
			violations = append(
				violations,
				"second Analyzer run changed semantic database signature",
			)
		}
	}
	semanticIdempotent := true
	if seeded {
		semanticIdempotent = runCount == 2
	} else {
		semanticIdempotent = reflect.DeepEqual(beforeReuse, firstSignature)
	}
	if !semanticIdempotent {
		violations = append(
			violations,
			"fresh reuse process changed semantic database signature",
		)
	}

	dbTotals := scaleDBTotals(t, conn)
	violations = append(violations, scaleDBTotalViolations(dbTotals)...)
	if seeded {
		publicWindow.CheckFinal(task4PublicSchemaSnapshot(t, conn))
		if !publicWindow.Unchanged() {
			violations = append(
				violations,
				"seed process changed public catalog over full lifecycle",
			)
		}
	}
	marker := scaleAcceptanceMarker{
		RunID:               runID,
		Schema:              schema,
		Seed:                1,
		Seeded:              seeded,
		Reused:              !seeded,
		PostgreSQLVersion:   version,
		PublicUnchanged:     false,
		CentralSQLRuns:      centralRuns,
		CopyChunkRows:       scaleCopyChunk,
		SeedDurationMS:      seedDuration,
		SeedChunks:          seedChunks,
		PhysicalRows:        physical,
		Plans:               plans,
		Runs:                runs,
		DBTotals:            dbTotals,
		SemanticIdempotent:  semanticIdempotent,
		PerformancePass:     false,
		PhysicalVerified:    true,
		SchemaPreserved:     false,
		CleanupPerformed:    false,
		CleanupResidual:     -1,
		SecondWindowsStatus: "USER_WAIVED",
	}

	if seeded && len(violations) == 0 {
		preserveSchema = true
		marker.SchemaPreserved = true
	} else {
		marker.CleanupResidual = scaleDropSchema(t, conn, runID, schema)
		marker.CleanupPerformed = true
		if marker.CleanupResidual == 0 {
			cleanupNeeded = false
		} else {
			violations = append(
				violations,
				fmt.Sprintf(
					"scale cleanup residual=%d",
					marker.CleanupResidual,
				),
			)
		}
		if !seeded && marker.CleanupResidual == 0 {
			publicWindow.CheckFinal(task4PublicSchemaSnapshot(t, conn))
			if !publicWindow.Unchanged() {
				violations = append(
					violations,
					"reuse process changed public catalog over full lifecycle",
				)
			}
		}
	}
	marker.PublicUnchanged = publicWindow.Unchanged()
	marker.PerformancePass = len(violations) == 0
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal scale marker: %v", err)
	}
	t.Logf("M3_SCALE_ACCEPTANCE %s", raw)
	if len(violations) != 0 {
		t.Fatalf("M3 scale acceptance breaches: %s", strings.Join(violations, "; "))
	}
}

func scaleValidateSchema(runID, schema string) error {
	expected := scaleSchemaForRunID(runID)
	if schema != expected {
		return fmt.Errorf(
			"FS_M3_SCHEMA=%q, want exact run-owned schema %q",
			schema,
			expected,
		)
	}
	if !regexp.MustCompile(`^m3_scale_[a-z0-9_]{8,96}$`).MatchString(schema) {
		return fmt.Errorf("unsafe M3 scale schema name %q", schema)
	}
	return nil
}

func scaleSchemaForRunID(runID string) string {
	var builder strings.Builder
	builder.WriteString("m3_scale_")
	for _, char := range strings.ToLower(runID) {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func scalePostgreSQLVersion(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var version string
	if err := conn.QueryRow(context.Background(), `SHOW server_version_num`).Scan(&version); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}
	if !strings.HasPrefix(version, "16") {
		t.Fatalf("PostgreSQL version_num=%s, want major 16", version)
	}
	return version
}

func scaleSchemaExists(t *testing.T, conn *pgx.Conn, schema string) bool {
	t.Helper()
	var exists bool
	if err := conn.QueryRow(
		context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`,
		schema,
	).Scan(&exists); err != nil {
		t.Fatalf("check scale schema existence: %v", err)
	}
	return exists
}

func scaleDropSchema(
	t *testing.T,
	conn *pgx.Conn,
	runID string,
	schema string,
) int64 {
	t.Helper()
	if err := scaleValidateSchema(runID, schema); err != nil {
		t.Errorf("refuse scale cleanup: %v", err)
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := conn.Exec(ctx, `SET search_path TO public`); err != nil {
		t.Errorf("cleanup set public search_path: %v", err)
		return -1
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoted+` CASCADE`); err != nil {
		t.Errorf("drop scale schema: %v", err)
		return -1
	}
	var residual int64
	if err := conn.QueryRow(
		ctx,
		`SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
		schema,
	).Scan(&residual); err != nil {
		t.Errorf("verify scale cleanup: %v", err)
		return -1
	}
	return residual
}

func scaleSeedDatabase(t *testing.T, conn *pgx.Conn) map[string]int {
	t.Helper()
	ctx := context.Background()
	chunks := map[string]int{}
	chunks["image_features"] = scaleCopyRows(
		t,
		conn,
		"image_features",
		[]string{"sha512", "width", "height", "pdq256", "pdq_quality"},
		&scaleImageRowGenerator{},
		scaleImageRows,
	)
	chunks["video_features"] = scaleCopyRows(
		t,
		conn,
		"video_features",
		[]string{"sha512", "duration_ms", "thumb_pdq256", "thumb_quality"},
		&scaleVideoRowGenerator{
			features: newScaleVideoGenerator(scaleVideoClusters),
		},
		scaleVideoRows,
	)
	chunks["files"] = scaleCopyRows(
		t,
		conn,
		"files",
		[]string{"machine_id", "disk_no", "path", "size", "sha512"},
		&scaleFileRowGenerator{},
		scaleFileRows,
	)
	for _, table := range []string{"files", "image_features", "video_features"} {
		if _, err := conn.Exec(ctx, `ANALYZE `+table); err != nil {
			t.Fatalf("analyze %s: %v", table, err)
		}
	}
	return chunks
}

func scaleCopyRows(
	t *testing.T,
	conn *pgx.Conn,
	table string,
	columns []string,
	generator scaleRowGenerator,
	total int,
) int {
	t.Helper()
	chunks := 0
	for copied := 0; copied < total; {
		count := min(scaleCopyChunk, total-copied)
		source := &scaleChunkSource{
			generator: generator,
			remaining: count,
		}
		inserted, err := conn.CopyFrom(
			context.Background(),
			pgx.Identifier{table},
			columns,
			source,
		)
		if err != nil {
			t.Fatalf("copy %s chunk %d: %v", table, chunks+1, err)
		}
		if inserted != int64(count) {
			t.Fatalf(
				"copy %s chunk %d inserted=%d, want %d",
				table,
				chunks+1,
				inserted,
				count,
			)
		}
		copied += count
		chunks++
	}
	return chunks
}

func scalePhysicalRows(t *testing.T, conn *pgx.Conn) map[string]int64 {
	t.Helper()
	result := make(map[string]int64, 3)
	for _, table := range []string{"files", "image_features", "video_features"} {
		var count int64
		if err := conn.QueryRow(
			context.Background(),
			`SELECT count(*) FROM `+table,
		).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		result[table] = count
	}
	return result
}

func scaleAssertPhysicalRows(t *testing.T, rows map[string]int64) {
	t.Helper()
	expected := map[string]int64{
		"files":          scaleFileRows,
		"image_features": scaleImageRows,
		"video_features": scaleVideoRows,
	}
	if !reflect.DeepEqual(rows, expected) {
		t.Fatalf("scale physical rows=%v, want %v", rows, expected)
	}
}

func scaleExplain(t *testing.T, conn *pgx.Conn, query string) scalePlanEvidence {
	t.Helper()
	var raw []byte
	if err := conn.QueryRow(
		context.Background(),
		`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+query,
	).Scan(&raw); err != nil {
		t.Fatalf("run scale EXPLAIN: %v", err)
	}
	var documents []map[string]any
	if err := json.Unmarshal(raw, &documents); err != nil {
		t.Fatalf("decode scale EXPLAIN: %v", err)
	}
	if len(documents) != 1 {
		t.Fatalf("EXPLAIN documents=%d, want 1", len(documents))
	}
	root, ok := documents[0]["Plan"].(map[string]any)
	if !ok {
		t.Fatal("EXPLAIN lacks root Plan object")
	}
	evidence := scalePlanEvidence{
		RootNodeType:     scaleJSONText(root, "Node Type"),
		ActualRows:       int64(scaleJSONNumber(root, "Actual Rows")),
		PlanningMS:       scaleJSONNumber(documents[0], "Planning Time"),
		ExecutionMS:      scaleJSONNumber(documents[0], "Execution Time"),
		SharedHitBlocks:  int64(scaleJSONNumber(root, "Shared Hit Blocks")),
		SharedReadBlocks: int64(scaleJSONNumber(root, "Shared Read Blocks")),
		Actual:           true,
	}
	indexSet := make(map[string]struct{})
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if name, ok := node["Index Name"].(string); ok && name != "" {
			indexSet[name] = struct{}{}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if object, ok := child.(map[string]any); ok {
				walk(object)
			}
		}
	}
	walk(root)
	for name := range indexSet {
		evidence.IndexNames = append(evidence.IndexNames, name)
	}
	sort.Strings(evidence.IndexNames)
	if evidence.RootNodeType == "" || evidence.ActualRows <= 0 {
		t.Fatalf("EXPLAIN evidence incomplete: %+v", evidence)
	}
	return evidence
}

func scaleJSONText(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func scaleJSONNumber(object map[string]any, key string) float64 {
	value, _ := object[key].(float64)
	return value
}

func scaleRunAnalyzer(
	t *testing.T,
	conn *pgx.Conn,
	ordinal int,
) (scaleRunEvidence, []string) {
	t.Helper()
	cfg := DefaultConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sampler := newScaleHeapSampler()
	started := time.Now()
	stats, err := NewAnalyzer(NewStore(conn, cfg), cfg, logger).Run(
		context.Background(),
	)
	total := time.Since(started)
	peak := sampler.Stop()
	if err != nil {
		t.Fatalf("scale Analyzer run %d: %v", ordinal, err)
	}
	stageKeys := make([]string, 0, len(stats.StageElapsedMs))
	for name := range stats.StageElapsedMs {
		stageKeys = append(stageKeys, name)
	}
	sort.Strings(stageKeys)
	run := scaleRunEvidence{
		Ordinal: ordinal,
		Counts: map[string]int{
			"files_scanned":   stats.FilesScanned,
			"exact_groups":    stats.ExactGroups,
			"exact_members":   stats.ExactMembers,
			"image_features":  stats.ImageFeatures,
			"image_pairs":     stats.ImagePairs,
			"video_features":  stats.VideoFeatures,
			"video_pairs":     stats.VideoPairs,
			"groups_written":  stats.GroupsWritten,
			"members_written": stats.MembersWritten,
			"skipped_pairs":   stats.SkippedPairs,
			"bad_rows":        stats.BadRows,
		},
		StageMS:      stats.StageElapsedMs,
		StageKeys:    stageKeys,
		TotalMS:      total.Milliseconds(),
		PeakHeapByte: peak,
	}
	t.Logf(
		"M3 scale run=%d total_ms=%d peak_heap_bytes=%d stages=%v counts=%v",
		ordinal,
		run.TotalMS,
		run.PeakHeapByte,
		run.StageMS,
		run.Counts,
	)
	return run, scaleRunViolations(run)
}

func scaleRunViolations(run scaleRunEvidence) []string {
	wantCounts := map[string]int{
		"files_scanned":   1_350_000,
		"exact_groups":    50_000,
		"exact_members":   150_000,
		"image_features":  990_400,
		"image_pairs":     60_000,
		"video_features":  200_000,
		"video_pairs":     15_000,
		"groups_written":  125_000,
		"members_written": 300_000,
		"skipped_pairs":   0,
		"bad_rows":        0,
	}
	var violations []string
	for name, want := range wantCounts {
		if run.Counts[name] != want {
			violations = append(
				violations,
				fmt.Sprintf("run %d %s=%d want=%d",
					run.Ordinal, name, run.Counts[name], want),
			)
		}
	}
	wantStages := []string{
		"db_write",
		"exact_group",
		"image_load",
		"image_screen",
		"video_load",
		"video_screen",
	}
	if !reflect.DeepEqual(run.StageKeys, wantStages) {
		violations = append(
			violations,
			fmt.Sprintf("run %d stage_keys=%v", run.Ordinal, run.StageKeys),
		)
	}
	if run.StageMS["image_screen"] > 5_000 {
		violations = append(
			violations,
			fmt.Sprintf("run %d image_screen=%dms >5000ms",
				run.Ordinal, run.StageMS["image_screen"]),
		)
	}
	if run.StageMS["video_screen"] > 3_000 {
		violations = append(
			violations,
			fmt.Sprintf("run %d video_screen=%dms >3000ms",
				run.Ordinal, run.StageMS["video_screen"]),
		)
	}
	if run.TotalMS > 90_000 {
		violations = append(
			violations,
			fmt.Sprintf("run %d total=%dms >90000ms",
				run.Ordinal, run.TotalMS),
		)
	}
	if run.PeakHeapByte > 4<<30 {
		violations = append(
			violations,
			fmt.Sprintf("run %d peak_heap=%d >4GiB",
				run.Ordinal, run.PeakHeapByte),
		)
	}
	return violations
}

func scaleSemanticSignature(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT kind,member_count,count(*)::bigint
		FROM dup_groups
		WHERE kind = ANY($1::text[])
		GROUP BY kind,member_count
		ORDER BY kind,member_count`,
		M3Kinds,
	)
	if err != nil {
		t.Fatalf("query scale semantic signature: %v", err)
	}
	defer rows.Close()
	var signature []string
	for rows.Next() {
		var kind string
		var members int
		var groups int64
		if err := rows.Scan(&kind, &members, &groups); err != nil {
			t.Fatalf("scan scale semantic signature: %v", err)
		}
		signature = append(signature, fmt.Sprintf("%s:%d:%d", kind, members, groups))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read scale semantic signature: %v", err)
	}
	return signature
}

func scaleDBTotals(t *testing.T, conn *pgx.Conn) map[string]int64 {
	t.Helper()
	result := map[string]int64{}
	var groupsTotal int64
	if err := conn.QueryRow(
		context.Background(),
		`SELECT count(*) FROM dup_groups WHERE kind = ANY($1::text[])`,
		M3Kinds,
	).Scan(&groupsTotal); err != nil {
		t.Fatalf("count scale groups: %v", err)
	}
	result["groups_total"] = groupsTotal
	var membersTotal int64
	if err := conn.QueryRow(
		context.Background(),
		`SELECT count(*) FROM dup_members m
		 JOIN dup_groups g ON g.id=m.group_id
		 WHERE g.kind = ANY($1::text[])`,
		M3Kinds,
	).Scan(&membersTotal); err != nil {
		t.Fatalf("count scale members: %v", err)
	}
	result["members_total"] = membersTotal
	rows, err := conn.Query(context.Background(), `
		SELECT kind,count(*)::bigint,COALESCE(sum(member_count),0)::bigint
		FROM dup_groups
		WHERE kind = ANY($1::text[])
		GROUP BY kind`,
		M3Kinds,
	)
	if err != nil {
		t.Fatalf("query scale kind totals: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var groups, members int64
		if err := rows.Scan(&kind, &groups, &members); err != nil {
			t.Fatalf("scan scale kind totals: %v", err)
		}
		result["groups_"+kind] = groups
		result["members_"+kind] = members
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read scale kind totals: %v", err)
	}
	return result
}

func scaleDBTotalViolations(got map[string]int64) []string {
	want := map[string]int64{
		"groups_total":            125_000,
		"members_total":           300_000,
		"groups_exact":            50_000,
		"members_exact":           150_000,
		"groups_image_candidate":  60_000,
		"members_image_candidate": 120_000,
		"groups_video_candidate":  15_000,
		"members_video_candidate": 30_000,
	}
	var violations []string
	for name, expected := range want {
		if got[name] != expected {
			violations = append(
				violations,
				fmt.Sprintf("database %s=%d want=%d", name, got[name], expected),
			)
		}
	}
	sort.Strings(violations)
	return violations
}
