package localanalysis

import (
	"context"
	"log/slog"

	"dedup/internal/firstscreen"
)

// StageOne runs the reusable first-screen algorithm against one local run.
type StageOne struct {
	source firstscreen.CandidateSource
	sink   firstscreen.CandidateSink
	cfg    firstscreen.Config
	log    *slog.Logger
}

func NewStageOne(source firstscreen.CandidateSource, sink firstscreen.CandidateSink, cfg firstscreen.Config, log *slog.Logger) *StageOne {
	return &StageOne{source: source, sink: sink, cfg: cfg, log: log}
}

func (s *StageOne) Run(ctx context.Context, machineID, runID string) (firstscreen.Result, error) {
	return s.analyzer(s.source).Run(ctx, machineID, runID)
}

func (s *StageOne) RunForRoots(ctx context.Context, machineID, runID string, roots []string) (firstscreen.Result, error) {
	source, err := newRootScopedCandidateSource(s.source, append([]string(nil), roots...))
	if err != nil {
		return firstscreen.Result{}, err
	}
	return s.analyzer(source).Run(ctx, machineID, runID)
}

func (s *StageOne) analyzer(source firstscreen.CandidateSource) *firstscreen.CandidateAnalyzer {
	return firstscreen.NewCandidateAnalyzer(source, s.sink, s.cfg, s.log)
}
