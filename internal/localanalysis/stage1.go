package localanalysis

import (
	"context"
	"log/slog"

	"dedup/internal/firstscreen"
)

// StageOne runs the reusable first-screen algorithm against one local run.
type StageOne struct {
	analyzer *firstscreen.CandidateAnalyzer
}

func NewStageOne(source firstscreen.CandidateSource, sink firstscreen.CandidateSink, cfg firstscreen.Config, log *slog.Logger) *StageOne {
	return &StageOne{analyzer: firstscreen.NewCandidateAnalyzer(source, sink, cfg, log)}
}

func (s *StageOne) Run(ctx context.Context, machineID, runID string) (firstscreen.Result, error) {
	return s.analyzer.Run(ctx, machineID, runID)
}
