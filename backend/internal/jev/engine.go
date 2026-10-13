package jev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

type modelClient interface {
	ResetCycle()
	Aborted() bool
	Decide(context.Context, DecisionRequest) (ModelAnswer, error)
}

type Engine struct {
	cfg                 config.Jev
	now                 func() time.Time
	store               Store
	market              MarketSource
	quotes              QuoteSource
	spots               SpotCatalog
	model               modelClient
	basis               *BasisCache
	onSnap              func(Snapshot)
	logger              *slog.Logger
	last                CandidateSet
	lastModel           ModelCycleStats
	lastEntryJobs       int
	lastEntryCandidates int
	lastMarketDataOK    bool
	lastDecisionSaved   atomic.Bool
}

func NewEngine(
	cfg config.Jev,
	store Store,
	market MarketSource,
	quotes QuoteSource,
	spots SpotCatalog,
	model modelClient,
	logger *slog.Logger,
) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		cfg: cfg, now: func() time.Time { return time.Now().UTC() },
		store: store, market: market, quotes: quotes, spots: spots, model: model, logger: logger,
		basis: NewBasisCache(cfg, quotes),
	}
}

func (e *Engine) SetSnapshotHandler(fn func(Snapshot)) { e.onSnap = fn }

func (e *Engine) RecoverAndStart(ctx context.Context) error {
	if _, err := e.ensureCurrentRun(ctx, e.now()); err != nil {
		return err
	}
	return e.RunCycle(ctx)
}

func (e *Engine) ensureCurrentRun(ctx context.Context, now time.Time) (Run, error) {
	return e.store.EnsureActiveRun(ctx, now, e.cfg.PaperInitialEquity, StrategyVersion)
}

func (e *Engine) RunLoop(ctx context.Context) {
	ticker := time.NewTicker(e.cfg.CycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.RunCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
				e.logger.Error("jev cycle failed", "error", err)
			}
		}
	}
}

func (e *Engine) RunCycle(ctx context.Context) error {
	lease, locked, err := e.store.AcquireCycleLock(ctx)
	if err != nil {
		return err
	}
	if !locked {
		e.logger.Info("jev cycle skipped: lock_busy")
		return nil
	}
	defer func() {
		if releaseErr := lease.Release(); releaseErr != nil {
			e.logger.Error("jev cycle lock release failed", "error", releaseErr)
		}
	}()
	if e.model != nil {
		e.model.ResetCycle()
	}
	e.last = CandidateSet{}
	e.lastModel = ModelCycleStats{Status: ModelStatusNotCalled}
	e.lastEntryJobs = 0
	e.lastEntryCandidates = 0
	e.lastMarketDataOK = false
	e.lastDecisionSaved.Store(false)
	started := time.Now()
	now := e.now().UTC()
	run, err := e.ensureCurrentRun(ctx, now)
	if err != nil {
		return err
	}
	degraded := make([]string, 0)
	if err := e.recoverPending(ctx, run, now, &degraded); err != nil {
		return err
	}
	if err := e.recoverOtherActiveRuns(ctx, run.ID, now, &degraded); err != nil {
		return err
	}
	if err := e.manageOpenPositions(ctx, run, now, &degraded); err != nil {
		return err
	}
	open, err := e.store.ListOpenPositions(ctx, run.ID)
	if err != nil {
		return err
	}
	others, err := e.store.ListOpenPositionsExceptStrategy(ctx, StrategyVersion)
	if err != nil {
		return err
	}
	if err := e.retryForcedAndPendingExits(ctx, append(append([]Position{}, open...), others...), now, &degraded); err != nil {
		return err
	}
	open, err = e.store.ListOpenPositions(ctx, run.ID)
	if err != nil {
		return err
	}
	status := StatusRunning
	if !e.typesafeEnabled() {
		status = StatusDisabled
	} else {
		if err := e.decideAndExecute(ctx, run, open, now, &degraded); err != nil {
			return err
		}
	}
	if len(degraded) > 0 && status == StatusRunning {
		status = StatusDegraded
	}
	e.logger.Info("jev cycle",
		"single_input", e.last.SingleStats.Input,
		"single_final", e.last.SingleStats.Final,
		"cross_input", e.last.CrossStats.Input,
		"cross_final", e.last.CrossStats.Final,
		"entry_jobs_capped", e.lastEntryJobs,
		"entry_candidates", e.lastEntryCandidates,
		"model_status", e.lastModel.Status,
		"typesafe_requests", e.lastModel.Requests,
		"typesafe_success", e.lastModel.Success,
		"typesafe_error", e.lastModel.Error,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)
	return e.publishStatus(ctx, status, degraded)
}

func (e *Engine) typesafeEnabled() bool {
	return e.cfg.Enabled && strings.TrimSpace(e.cfg.TypeSafeAPIKey) != "" && e.model != nil
}

func (e *Engine) recoverPending(ctx context.Context, run Run, now time.Time, degraded *[]string) error {
	pending, err := e.store.ListPendingDecisions(ctx, run.ID)
	if err != nil {
		return err
	}
	open, err := e.store.ListOpenPositions(ctx, run.ID)
	if err != nil {
		return err
	}
	byID := indexPositions(open)
	for _, decision := range pending {
		if decision.FillDeadline == nil || now.After(*decision.FillDeadline) {
			decision.ExecutionStatus = ExecExpired
			if err := e.saveDecision(ctx, decision); err != nil {
				return err
			}
			if decision.Decision == DecisionExit && decision.PositionID != nil {
				if position, ok := byID[*decision.PositionID]; ok && position.Status == PositionOpen {
					if err := e.store.SavePosition(ctx, position); err != nil {
						return err
					}
				}
			}
			continue
		}
		if err := e.executeDecision(ctx, run, decision, byID, now, degraded); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) recoverOtherActiveRuns(ctx context.Context, currentRunID int64, now time.Time, degraded *[]string) error {
	runs, err := e.store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, item := range runs {
		if item.Status != "active" || item.ID == currentRunID {
			continue
		}
		if err := e.recoverPending(ctx, item, now, degraded); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) manageOpenPositions(ctx context.Context, run Run, now time.Time, degraded *[]string) error {
	open, err := e.store.ListOpenPositions(ctx, run.ID)
	if err != nil {
		return err
	}
	others, err := e.store.ListOpenPositionsExceptStrategy(ctx, StrategyVersion)
	if err != nil {
		return err
	}
	all := append(append([]Position{}, open...), others...)
	quotes, err := e.latestQuotes(ctx, canonicalPositionKeys(all), now, "positions")
	if err != nil {
		*degraded = appendUnique(*degraded, "quote_query_failed")
		quotes = map[string]Quote{}
	}
	rates, ratesErr := e.market.ListRates(ctx)
	if ratesErr != nil {
		rates = nil
	}
	for i := range all {
		position := all[i]
		if !position.CanonicalOK() {
			position.DegradedReason = CanonicalUnresolved
			*degraded = appendUnique(*degraded, CanonicalUnresolved)
			if err := e.store.SavePosition(ctx, position); err != nil {
				return err
			}
			continue
		}
		position = e.catchupFunding(ctx, position, now, degraded)
		long, _ := quotes[position.LongKey().QuoteKey()]
		short, _ := quotes[position.ShortKey().QuoteKey()]
		position = MarkToMarket(position, long, short, now, e.cfg.BBOMaxAge)
		fundingReturn := decimal.NewFromInt(1)
		if ratesErr == nil {
			fundingReturn = fundingReturnFromRates(position, rates)
		}
		position = ApplyExitState(e.cfg, position, fundingReturn, long, short, now)
		if err := e.store.SavePosition(ctx, position); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) retryForcedAndPendingExits(
	ctx context.Context,
	open []Position,
	now time.Time,
	degraded *[]string,
) error {
	byID := indexPositions(open)
	pendingExit := map[int64]struct{}{}
	runIDs := map[int64]struct{}{}
	for _, position := range open {
		runIDs[position.RunID] = struct{}{}
	}
	for runID := range runIDs {
		pending, err := e.store.ListPendingDecisions(ctx, runID)
		if err != nil {
			return err
		}
		for _, decision := range pending {
			if decision.Decision == DecisionExit && decision.PositionID != nil && decision.ExecutionStatus == ExecPending {
				pendingExit[*decision.PositionID] = struct{}{}
			}
		}
	}
	quotes, err := e.latestQuotes(ctx, canonicalPositionKeys(open), now, "forced-exit")
	if err != nil {
		*degraded = appendUnique(*degraded, "quote_query_failed")
		quotes = map[string]Quote{}
	}
	for _, position := range open {
		if _, exists := pendingExit[position.ID]; exists {
			continue
		}
		long, _ := quotes[position.LongKey().QuoteKey()]
		short, _ := quotes[position.ShortKey().QuoteKey()]
		if !ShouldFlattenNow(e.cfg, position, long, short, now) {
			continue
		}
		started := now
		deadline := now.Add(e.cfg.FillWait)
		promptVersion := "system-local-exit"
		rationale := "local take-profit confirmed"
		if position.ExitPendingReason == MandatoryExitPending {
			promptVersion = "system-force-exit"
			rationale = "mandatory exit"
		}
		decision, err := e.insertDecision(ctx, Decision{
			RunID: position.RunID, PositionID: ptrInt(position.ID), CandidateKey: position.CandidateKey,
			Kind: position.Kind, Decision: DecisionExit, PolicyStatus: PolicyAccepted,
			ExecutionStatus: ExecPending, ExitProbability: decimal.NewFromInt(1),
			Confidence: decimal.NewFromInt(1), Rationale: rationale,
			PromptVersion: promptVersion, PolicyVersion: PolicyVersion, StrategyVersion: position.StrategyVersion,
			DecisionAt: now, ExecutionStartedAt: &started, FillDeadline: &deadline,
			BaseAsset: position.BaseAsset, QuoteAsset: position.QuoteAsset,
			LongCanonicalSymbol: position.LongCanonicalSymbol, LongQuoteAsset: position.LongQuoteAsset,
			ShortCanonicalSymbol: position.ShortCanonicalSymbol, ShortQuoteAsset: position.ShortQuoteAsset,
		})
		if err != nil {
			return err
		}
		if err := e.executeDecision(ctx, Run{ID: position.RunID}, decision, byID, now, degraded); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) decideAndExecute(
	ctx context.Context,
	run Run,
	open []Position,
	now time.Time,
	degraded *[]string,
) error {
	rates, spots, err := e.loadUniverse(ctx, degraded)
	if err != nil {
		return err
	}
	fundingSingle, fundingCross, singleStats, crossStats := SelectFundingUniverse(e.cfg, now, rates, spots)
	fundingAll := append(append([]Candidate{}, fundingSingle...), fundingCross...)
	keys := append(candidateKeys(fundingAll), canonicalPositionKeys(open)...)
	quotes, err := e.latestQuotes(ctx, keys, now, "universe")
	candidateNow := e.now().UTC()
	if err != nil {
		*degraded = appendUnique(*degraded, "quote_query_failed")
		quotes = map[string]Quote{}
	}
	e.lastMarketDataOK = err == nil
	singleFiltered, singleAvail, singleFresh, singleDepth := FilterByLatestBBO(e.cfg, candidateNow, fundingSingle, quotes, KindSingle)
	crossFiltered, crossAvail, crossFresh, crossDepth := FilterByLatestBBO(e.cfg, candidateNow, fundingCross, quotes, KindCross)
	singleStats.BBOAvailable = singleAvail
	singleStats.BBOFreshPassed = singleFresh
	singleStats.DepthPassed = singleDepth
	crossStats.BBOAvailable = crossAvail
	crossStats.BBOFreshPassed = crossFresh
	crossStats.DepthPassed = crossDepth
	phase2 := append(append([]Candidate{}, singleFiltered...), crossFiltered...)
	if err := e.basis.MaybeRefresh(ctx, candidateNow, phase2, open); err != nil {
		*degraded = appendUnique(*degraded, "basis_forecast_unavailable")
	}
	lookup := func(long, short MarketKey) (BasisForecast, bool) {
		return e.lookupForecast(candidateNow, long, short)
	}
	singleFinal, singleBasis, singlePayback := FinalizeEntryCandidates(e.cfg, candidateNow, singleFiltered, lookup)
	crossFinal, crossBasis, crossPayback := FinalizeEntryCandidates(e.cfg, candidateNow, crossFiltered, lookup)
	singleStats.BasisForecastPassed = singleBasis
	singleStats.PaybackPassed = singlePayback
	singleStats.Final = len(singleFinal)
	crossStats.BasisForecastPassed = crossBasis
	crossStats.PaybackPassed = crossPayback
	crossStats.Final = len(crossFinal)
	set := CandidateSet{
		Single: singleFinal, Cross: crossFinal,
		SingleEmptyReason: singleEmptyReason(singleStats), CrossEmptyReason: crossEmptyReason(crossStats),
		SingleStats: singleStats, CrossStats: crossStats,
	}
	jobs := make([]decisionJob, 0, len(open)+len(set.Single)+len(set.Cross))
	openBases := map[string]struct{}{}
	for _, position := range open {
		openBases[strings.ToUpper(position.BaseAsset)] = struct{}{}
		if position.ExitPendingReason == MandatoryExitPending ||
			(e.cfg.ExitConfirmCycles > 0 && position.ExitConfirmCount >= e.cfg.ExitConfirmCycles) {
			continue
		}
		jobs = append(jobs, decisionJob{
			candidate:  candidateFromPosition(position, quotes, rates, e.cfg, candidateNow, lookup),
			position:   clonePosition(position),
			allowEnter: false,
		})
	}
	eligible := make([]Candidate, 0, len(set.Single)+len(set.Cross))
	for _, candidate := range AllEntryCandidates(set) {
		if _, exists := openBases[strings.ToUpper(candidate.BaseAsset)]; exists {
			continue
		}
		eligible = append(eligible, candidate)
	}
	e.lastEntryCandidates = len(eligible)
	capped := capEntryCandidates(eligible, e.cfg.MaxEntryDecisionsPerCycle)
	e.lastEntryJobs = len(capped)
	for _, candidate := range capped {
		jobs = append(jobs, decisionJob{candidate: candidate, allowEnter: true})
	}
	decisions, stats := e.runDecisionJobs(ctx, run, jobs, now, degraded)
	e.lastModel = stats
	e.last = set
	justExited := map[string]struct{}{}
	byID := indexPositions(open)
	for _, decision := range decisions {
		if decision.Decision != DecisionExit || decision.PositionID == nil {
			continue
		}
		if decision.ExecutionStatus != ExecPending {
			continue
		}
		before, ok := byID[*decision.PositionID]
		if !ok {
			continue
		}
		if err := e.executeDecision(ctx, run, decision, byID, now, degraded); err != nil {
			return err
		}
		updated, err := e.store.ListOpenPositions(ctx, run.ID)
		if err != nil {
			return err
		}
		stillOpen := false
		for _, item := range updated {
			if item.ID == before.ID {
				stillOpen = true
				break
			}
		}
		if !stillOpen {
			justExited[strings.ToUpper(before.BaseAsset)] = struct{}{}
		}
	}
	open, err = e.store.ListOpenPositions(ctx, run.ID)
	if err != nil {
		return err
	}
	enters := make([]Decision, 0)
	for _, decision := range decisions {
		if decision.Decision == DecisionEnter {
			enters = append(enters, decision)
		}
	}
	allocated := AllocateEnters(e.cfg, open, enters, justExited)
	e.last = set
	for _, decision := range allocated {
		if decision.ID == 0 {
			continue
		}
		if decision.ExecutionStatus == ExecPending {
			if err := e.executeDecision(ctx, run, decision, map[int64]Position{}, now, degraded); err != nil {
				return err
			}
			continue
		}
		if err := e.saveDecision(ctx, decision); err != nil {
			return err
		}
	}
	e.last = set
	return nil
}

type decisionJob struct {
	candidate  Candidate
	position   *Position
	allowEnter bool
	forceExit  bool
}

func (e *Engine) runDecisionJobs(
	ctx context.Context,
	run Run,
	jobs []decisionJob,
	now time.Time,
	degraded *[]string,
) ([]Decision, ModelCycleStats) {
	out := make([]Decision, 0, len(jobs))
	stats := ModelCycleStats{DecisionJobs: len(jobs), Status: ModelStatusNotCalled}
	var mu sync.Mutex
	sem := make(chan struct{}, max(1, e.cfg.MaxConcurrentDecisions))
	var wg sync.WaitGroup
	recordCall := func(err error, modelActual string) {
		callAt := now
		stats.Requests++
		stats.LastModelCallAt = &callAt
		if err != nil {
			stats.Error++
			if errors.Is(err, ErrUnauthorized) {
				stats.Status = mergeModelStatus(stats.Status, ModelStatusUnauthorized)
				*degraded = appendUnique(*degraded, "typesafe_unauthorized")
				return
			}
			stats.Status = mergeModelStatus(stats.Status, ModelStatusError)
			*degraded = appendUnique(*degraded, "typesafe_error")
			return
		}
		stats.Success++
		stats.Status = mergeModelStatus(stats.Status, ModelStatusSuccess)
		if strings.TrimSpace(modelActual) != "" {
			stats.ModelActual = modelActual
		}
	}
	for _, job := range jobs {
		if e.model != nil && e.model.Aborted() {
			mu.Lock()
			*degraded = appendUnique(*degraded, "typesafe_unauthorized")
			stats.Status = mergeModelStatus(stats.Status, ModelStatusUnauthorized)
			mu.Unlock()
			break
		}
		if job.forceExit {
			started := now
			deadline := now.Add(e.cfg.FillWait)
			var positionID *int64
			base := job.candidate.BaseAsset
			if job.position != nil {
				positionID = ptrInt(job.position.ID)
				base = job.position.BaseAsset
			}
			decision, err := e.insertDecision(ctx, Decision{
				RunID: run.ID, PositionID: positionID, CandidateKey: job.candidate.Key,
				Kind: job.candidate.Kind, Decision: DecisionExit, PolicyStatus: PolicyAccepted,
				ExecutionStatus: ExecPending, ExitProbability: decimal.NewFromInt(1),
				Confidence: decimal.NewFromInt(1), Rationale: "max hold duration reached",
				PromptVersion: "system-force-exit", PolicyVersion: PolicyVersion, StrategyVersion: StrategyVersion,
				DecisionAt: now, ExecutionStartedAt: &started, FillDeadline: &deadline, BaseAsset: base,
				QuoteAsset:          job.candidate.QuoteAsset,
				LongCanonicalSymbol: job.candidate.LongCanonicalSymbol, LongQuoteAsset: job.candidate.LongQuoteAsset,
				ShortCanonicalSymbol: job.candidate.ShortCanonicalSymbol, ShortQuoteAsset: job.candidate.ShortQuoteAsset,
			})
			if err == nil {
				mu.Lock()
				out = append(out, decision)
				mu.Unlock()
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		if e.model != nil && e.model.Aborted() {
			<-sem
			wg.Done()
			mu.Lock()
			*degraded = appendUnique(*degraded, "typesafe_unauthorized")
			stats.Status = mergeModelStatus(stats.Status, ModelStatusUnauthorized)
			mu.Unlock()
			break
		}
		go func(job decisionJob) {
			defer wg.Done()
			defer func() { <-sem }()
			decision, err := e.askModel(ctx, run, job, now)
			mu.Lock()
			recordCall(err, decision.ModelActual)
			mu.Unlock()
			if err != nil {
				if job.position != nil {
					hold := e.holdDecision(run, job, now, err.Error())
					if saved, saveErr := e.insertDecision(ctx, hold); saveErr == nil {
						mu.Lock()
						out = append(out, saved)
						mu.Unlock()
					}
				}
				return
			}
			saved, err := e.insertDecision(ctx, decision)
			if err != nil {
				return
			}
			mu.Lock()
			out = append(out, saved)
			mu.Unlock()
		}(job)
	}
	wg.Wait()
	return out, stats
}

func mergeModelStatus(current, next string) string {
	if modelStatusRank(next) > modelStatusRank(current) {
		return next
	}
	return current
}

func modelStatusRank(status string) int {
	switch status {
	case ModelStatusUnauthorized:
		return 3
	case ModelStatusError:
		return 2
	case ModelStatusSuccess:
		return 1
	default:
		return 0
	}
}

func (e *Engine) askModel(ctx context.Context, run Run, job decisionJob, now time.Time) (Decision, error) {
	if e.model != nil && e.model.Aborted() {
		return Decision{}, ErrUnauthorized
	}
	timeout, cancel := context.WithTimeout(ctx, e.cfg.TypeSafeTimeout)
	defer cancel()
	prompt := BuildDecisionPrompt(e.cfg, job.candidate, job.position, now, job.forceExit)
	answer, err := e.model.Decide(timeout, DecisionRequest{
		Prompt: prompt, AllowEnter: job.allowEnter, ForceExit: job.forceExit,
	})
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		RunID: run.ID, CandidateKey: job.candidate.Key, Kind: job.candidate.Kind,
		Decision: answer.Decision, PolicyStatus: PolicyAccepted, ExecutionStatus: ExecNotApplicable,
		EnterProbability: answer.EnterProbability, ExitProbability: answer.ExitProbability,
		Confidence: answer.Confidence, Rationale: formatDecisionRationale(answer),
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, StrategyVersion: StrategyVersion,
		ModelRequested: e.cfg.TypeSafeModel, ModelActual: answer.ModelActual, RawResponse: SanitizeJSONBytes(answer.Raw),
		ParseErrorType: answer.ParseErrorType,
		DecisionAt:     now, BaseAsset: job.candidate.BaseAsset, QuoteAsset: job.candidate.QuoteAsset,
		LongCanonicalSymbol: job.candidate.LongCanonicalSymbol, LongQuoteAsset: job.candidate.LongQuoteAsset,
		ShortCanonicalSymbol: job.candidate.ShortCanonicalSymbol, ShortQuoteAsset: job.candidate.ShortQuoteAsset,
	}
	copyLocalMetrics(&decision, job.candidate, job.position)
	if job.position != nil {
		decision.PositionID = ptrInt(job.position.ID)
		decision.BaseAsset = job.position.BaseAsset
		if (ShouldHonorExit(e.cfg, answer) || job.forceExit) && TakeProfitReady(e.cfg, *job.position) {
			decision.Decision = DecisionExit
			decision.ExecutionStatus = ExecPending
		} else {
			decision.Decision = DecisionHold
			if answer.Decision == DecisionExit && !TakeProfitReady(e.cfg, *job.position) {
				decision.Rationale = formatDecisionRationale(answer) + "; local_rules_block_model_exit"
			}
		}
	} else if answer.Decision == DecisionEnter {
		if reason := EnterRejectReason(e.cfg, ModelAnswer{
			Decision: answer.Decision, ExpectedNetReturn24h: decision.ExpectedNetReturn24h,
			EnterProbability: answer.EnterProbability, Confidence: answer.Confidence,
		}); reason != "" {
			decision.PolicyStatus = PolicyRejected
			decision.ExecutionStatus = ExecNotApplicable
			decision.ErrorMessage = reason
		} else {
			decision.PolicyStatus = PolicySuggested
			decision.ExecutionStatus = ExecNotApplicable
		}
	}
	return decision, nil
}

func (e *Engine) holdDecision(run Run, job decisionJob, now time.Time, message string) Decision {
	decision := Decision{
		RunID: run.ID, CandidateKey: job.candidate.Key, Kind: job.candidate.Kind,
		Decision: DecisionHold, PolicyStatus: PolicyAccepted, ExecutionStatus: ExecNotApplicable,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, StrategyVersion: StrategyVersion, DecisionAt: now,
		Rationale: ModelFailedLocalRules, ErrorMessage: ModelFailedLocalRules,
		ParseErrorType: "model_call_failed", RawResponse: FailureRawResponse("model_call_failed", message),
		BaseAsset: job.candidate.BaseAsset,
	}
	if job.position != nil {
		decision.PositionID = ptrInt(job.position.ID)
		decision.BaseAsset = job.position.BaseAsset
		decision.QuoteAsset = job.position.QuoteAsset
		decision.LongCanonicalSymbol = job.position.LongCanonicalSymbol
		decision.LongQuoteAsset = job.position.LongQuoteAsset
		decision.ShortCanonicalSymbol = job.position.ShortCanonicalSymbol
		decision.ShortQuoteAsset = job.position.ShortQuoteAsset
	}
	return decision
}

func (e *Engine) executeDecision(
	ctx context.Context,
	run Run,
	decision Decision,
	open map[int64]Position,
	_ time.Time,
	degraded *[]string,
) error {
	if decision.ExecutionStatus != ExecPending {
		return nil
	}
	executionNow := e.now().UTC()
	if decision.FillDeadline != nil && executionNow.After(*decision.FillDeadline) {
		decision.ExecutionStatus = ExecExpired
		return e.saveDecision(ctx, decision)
	}
	if decision.FillDeadline == nil {
		started := executionNow
		deadline := executionNow.Add(e.cfg.FillWait)
		if decision.ExecutionStartedAt == nil {
			decision.ExecutionStartedAt = &started
		}
		decision.FillDeadline = &deadline
		if err := e.saveDecision(ctx, decision); err != nil {
			return err
		}
	}
	keys := make([]MarketKey, 0, 2)
	var position Position
	var candidate Candidate
	if decision.Decision == DecisionExit && decision.PositionID != nil {
		var ok bool
		position, ok = open[*decision.PositionID]
		if !ok {
			loaded, err := e.store.ListOpenPositions(ctx, run.ID)
			if err != nil {
				return err
			}
			open = indexPositions(loaded)
			position, ok = open[*decision.PositionID]
			if !ok {
				decision.ExecutionStatus = ExecExpired
				return e.saveDecision(ctx, decision)
			}
		}
		if !position.CanonicalOK() {
			*degraded = appendUnique(*degraded, CanonicalUnresolved)
			return e.failFill(ctx, decision, position, fmt.Errorf(CanonicalUnresolved), executionNow, Quote{}, Quote{}, degraded)
		}
		keys = append(keys, position.LongKey(), position.ShortKey())
	} else if decision.Decision == DecisionEnter {
		parsed, ok := parseCandidateKey(decision.CandidateKey)
		if !ok {
			decision.ExecutionStatus = ExecExpired
			return e.saveDecision(ctx, decision)
		}
		candidate = parsed
		candidate.BaseAsset = decision.BaseAsset
		candidate.QuoteAsset = decision.QuoteAsset
		candidate.LongCanonicalSymbol = decision.LongCanonicalSymbol
		candidate.LongQuoteAsset = decision.LongQuoteAsset
		candidate.ShortCanonicalSymbol = decision.ShortCanonicalSymbol
		candidate.ShortQuoteAsset = decision.ShortQuoteAsset
		if !candidate.CanonicalOK() {
			decision.ExecutionStatus = ExecFillFailed
			decision.ErrorMessage = CanonicalUnresolved
			*degraded = appendUnique(*degraded, CanonicalUnresolved)
			return e.saveDecision(ctx, decision)
		}
		keys = append(keys, candidate.LongKey(), candidate.ShortKey())
	} else {
		decision.ExecutionStatus = ExecNotApplicable
		return e.saveDecision(ctx, decision)
	}
	quotes, err := e.latestQuotes(ctx, keys, executionNow, "execution")
	fillNow := e.now().UTC()
	if err != nil {
		*degraded = appendUnique(*degraded, "quote_query_failed")
		return e.failFill(ctx, decision, position, err, fillNow, Quote{}, Quote{}, degraded)
	}
	long, _ := quotes[keys[0].QuoteKey()]
	short, _ := quotes[keys[1].QuoteKey()]
	if decision.Decision == DecisionEnter {
		fill, err := EnterFill(e.cfg, fillNow, long, short)
		if err != nil {
			return e.failFill(ctx, decision, Position{}, err, fillNow, long, short, degraded)
		}
		position := ApplyEnter(run.ID, candidate, fill, fillNow, long, short)
		saved, err := e.store.InsertPosition(ctx, position)
		if err != nil {
			return err
		}
		decision.ExecutionStatus = ExecFilled
		decision.PositionID = ptrInt(saved.ID)
		if err := e.saveDecision(ctx, decision); err != nil {
			return err
		}
		_, err = e.store.InsertTrade(ctx, Trade{
			RunID: run.ID, PositionID: saved.ID, DecisionID: ptrInt(decision.ID), Side: DecisionEnter,
			SharedBaseQty: fill.Qty, LongFillPrice: fill.LongPrice, ShortFillPrice: fill.ShortPrice,
			Notional: fill.Notional, Cost: fill.Cost, FilledAt: fillNow,
		})
		return err
	}
	fill, err := ExitFill(e.cfg, fillNow, position, long, short)
	if err != nil {
		return e.failFill(ctx, decision, position, err, fillNow, long, short, degraded)
	}
	updated := ApplyExit(position, fill, fillNow)
	if err := e.store.SavePosition(ctx, updated); err != nil {
		return err
	}
	decision.ExecutionStatus = ExecFilled
	if err := e.saveDecision(ctx, decision); err != nil {
		return err
	}
	_, err = e.store.InsertTrade(ctx, Trade{
		RunID: run.ID, PositionID: updated.ID, DecisionID: ptrInt(decision.ID), Side: DecisionExit,
		SharedBaseQty: fill.Qty, LongFillPrice: fill.LongPrice, ShortFillPrice: fill.ShortPrice,
		Notional: fill.Notional, Cost: fill.Cost, FilledAt: fillNow,
	})
	return err
}

func (e *Engine) failFill(
	ctx context.Context,
	decision Decision,
	position Position,
	fillErr error,
	fillNow time.Time,
	long Quote,
	short Quote,
	degraded *[]string,
) error {
	if fillErr != nil {
		decision.ErrorMessage = fillErr.Error()
	}
	if decision.Decision == DecisionExit && position.ID != 0 {
		if err := e.store.SavePosition(ctx, position); err != nil {
			return err
		}
		*degraded = appendUnique(*degraded, ExitPendingMarketData)
		return e.saveDecision(ctx, decision)
	}
	decision.ExecutionStatus = ExecFillFailed
	e.logger.Warn("jev enter fill failed",
		"candidate", decision.CandidateKey,
		"long_bbo_ts", long.TS,
		"short_bbo_ts", short.TS,
		"fill_now", fillNow,
		"reason", decision.ErrorMessage,
	)
	return e.saveDecision(ctx, decision)
}

func parseCandidateKey(key string) (Candidate, bool) {
	parts := strings.Split(key, ":")
	if len(parts) == 4 && parts[0] == KindSingle {
		return Candidate{
			Key: key, Kind: KindSingle,
			LongExchange: parts[1], LongSymbol: parts[2], LongProduct: ProductSpot,
			ShortExchange: parts[1], ShortSymbol: parts[3], ShortProduct: ProductPerpetual,
		}, true
	}
	if len(parts) == 5 && parts[0] == KindCross {
		return Candidate{
			Key: key, Kind: KindCross,
			LongExchange: parts[1], LongSymbol: parts[2], LongProduct: ProductPerpetual,
			ShortExchange: parts[3], ShortSymbol: parts[4], ShortProduct: ProductPerpetual,
		}, true
	}
	return Candidate{}, false
}

func copyLocalMetrics(decision *Decision, candidate Candidate, position *Position) {
	decision.ExpectedNetReturn24h = decimalPtr(candidate.ExpectedNetReturn24h)
	decision.FundingReturn24h = decimalPtr(candidate.FundingReturn24h)
	decision.HoldingHorizonNetReturn = decimalPtr(candidate.HoldingHorizonNetReturn)
	decision.ExpectedPaybackMinutes = candidate.ExpectedPaybackMinutes
	decision.CurrentMidBasisBPS = candidate.CurrentMidBasisBPS
	decision.ExpectedBasisReturn24h = candidate.ExpectedBasisReturn24h
	decision.BasisReturnP10 = candidate.BasisReturnP10
	decision.BasisProfitProbability = candidate.BasisProfitProbability
	decision.BasisForecastConfidence = candidate.BasisForecastConfidence
	decision.BasisSampleCount = candidate.BasisSampleCount
	decision.BasisModelVersion = candidate.BasisModelVersion
	decision.BasisForecastCalculatedAt = candidate.BasisForecastCalculatedAt
	if position != nil {
		decision.EntryMidBasisBPS = position.EntryMidBasisBPS
		decision.BasisChangeSinceEntryBPS = BasisChangeSinceEntryBPS(candidate.CurrentMidBasisBPS, position.EntryMidBasisBPS)
	}
}

func (e *Engine) lookupForecast(now time.Time, long, short MarketKey) (BasisForecast, bool) {
	if e.basis == nil {
		return BasisForecast{}, false
	}
	item, ok := e.basis.LookupAny(long, short)
	if !ok || item.err != nil || !item.forecast.OK {
		return BasisForecast{}, false
	}
	if now.Sub(item.forecast.CalculatedAt) > e.cfg.BasisCacheMaxAge {
		return item.forecast, false
	}
	return item.forecast, true
}

func (e *Engine) catchupFunding(ctx context.Context, position Position, now time.Time, degraded *[]string) Position {
	complete := true
	if position.ShortProduct == ProductPerpetual {
		var ok bool
		position, ok = e.catchupLeg(ctx, position, false, now)
		complete = complete && ok
	}
	if position.LongProduct == ProductPerpetual {
		var ok bool
		position, ok = e.catchupLeg(ctx, position, true, now)
		complete = complete && ok
	}
	if !complete {
		*degraded = appendUnique(*degraded, AccountingIncomplete)
		position.DegradedReason = AccountingIncomplete
	} else if position.DegradedReason == AccountingIncomplete {
		position.DegradedReason = ""
	}
	return position
}

func (e *Engine) catchupLeg(ctx context.Context, position Position, long bool, now time.Time) (Position, bool) {
	exchange, symbol := position.ShortExchange, position.ShortSymbol
	watermark := position.FundingCheckedThroughShort
	qty := position.ShortQty
	key := position.ShortKey()
	if long {
		exchange, symbol = position.LongExchange, position.LongSymbol
		watermark = position.FundingCheckedThroughLong
		qty = position.LongQty
		key = position.LongKey()
	}
	points, err := e.market.ListHistory(ctx, exchange, symbol, HistoryLimit)
	if err != nil {
		return position, false
	}
	if HistoryTruncated(watermark, position.OpenedAt, points, HistoryLimit) {
		return position, false
	}
	boundary := FundingBoundary(watermark, position.OpenedAt)
	for _, point := range SortHistoryAscending(points) {
		if !point.SettledAt.After(boundary) || point.SettledAt.After(now) {
			continue
		}
		mid, ts, ok, err := e.quotes.MidBefore(ctx, key, point.SettledAt, e.cfg.FundingPriceMaxAge)
		if err != nil || !ok || !SettlementMidValid(point.SettledAt, ts, e.cfg.FundingPriceMaxAge) {
			if long {
				position.FundingCheckedThroughLong = watermark
			} else {
				position.FundingCheckedThroughShort = watermark
			}
			return position, false
		}
		position.FundingPnL = position.FundingPnL.Add(FundingPnLDelta(qty, mid, point.Rate))
		watermark = NextWatermark(watermark, point.SettledAt)
		boundary = FundingBoundary(watermark, position.OpenedAt)
	}
	if long {
		position.FundingCheckedThroughLong = watermark
	} else {
		position.FundingCheckedThroughShort = watermark
	}
	return position, true
}

func (e *Engine) loadUniverse(ctx context.Context, degraded *[]string) ([]Rate, map[string]SpotInstrument, error) {
	rates, err := e.market.ListRates(ctx)
	if err != nil {
		return nil, nil, err
	}
	spots, err := e.spots.ListSpots(ctx)
	if err != nil {
		*degraded = appendUnique(*degraded, "spot_catalog_unavailable")
		spots = map[string]SpotInstrument{}
	}
	return rates, spots, nil
}

func (e *Engine) latestQuotes(ctx context.Context, keys []MarketKey, now time.Time, scope string) (map[string]Quote, error) {
	if len(keys) == 0 {
		return map[string]Quote{}, nil
	}
	quotes, err := e.quotes.Latest(ctx, keys)
	if err != nil {
		e.logger.Error("jev quote query failed", "scope", scope, "error", err)
		return nil, err
	}
	_ = now
	return quotes, nil
}

func (e *Engine) publish(ctx context.Context, reasons []string, status string) error {
	return e.publishStatus(ctx, status, reasons)
}

func (e *Engine) publishStatus(ctx context.Context, status string, reasons []string) error {
	now := e.now().UTC()
	run, err := e.ensureCurrentRun(ctx, now)
	if err != nil {
		return err
	}
	positions, err := e.store.ListAllPositions(ctx, run.ID)
	if err != nil {
		return err
	}
	run = RecomputeRunTotals(run, positions)
	run.LastSuccessfulAt = &now
	if e.lastMarketDataOK {
		run.LastMarketDataAt = &now
	}
	if e.lastDecisionSaved.Load() {
		run.LastDecisionAt = &now
	}
	if err := e.store.SaveRun(ctx, run); err != nil {
		return err
	}
	if err := e.persistOtherRunTotals(ctx, run.ID); err != nil {
		return err
	}
	if err := e.store.InsertEquityPoint(ctx, run.ID, EquityPoint{TS: now, Equity: run.Equity, NetPnL: run.NetPnL}); err != nil {
		return err
	}
	openCount := 0
	for _, position := range positions {
		if position.Status == PositionOpen {
			openCount++
		}
	}
	runs, err := e.store.ListRuns(ctx)
	if err != nil {
		return err
	}
	lifeEquity, lifeNet, lifeFunding, lifeRealized, lifeUnrealized, lifeCost := lifecycleFromRuns(runs)
	snapshot := Snapshot{
		Status: status, DegradedReasons: reasons, LastSuccessfulAt: run.LastSuccessfulAt,
		LastDecisionAt: run.LastDecisionAt, LastMarketDataAt: run.LastMarketDataAt, ServerTime: now,
		Model: e.lastModel.ModelActual, ModelRequested: e.cfg.TypeSafeModel, PromptVersion: PromptVersion,
		PolicyVersion: PolicyVersion, StrategyVersion: StrategyVersion, Simulated: true, Equity: run.Equity, NetPnL: run.NetPnL,
		FundingPnL: run.FundingPnL, RealizedSpreadPnL: run.RealizedSpreadPnL,
		UnrealizedSpreadPnL: run.UnrealizedSpreadPnL, CumulativeCost: run.CumulativeCost,
		MaxDrawdown: run.MaxDrawdown, InitialEquity: run.InitialEquity,
		LifecycleEquity: lifeEquity, LifecycleNetPnL: lifeNet, LifecycleFundingPnL: lifeFunding,
		LifecycleRealizedSpreadPnL: lifeRealized, LifecycleUnrealizedSpreadPnL: lifeUnrealized,
		LifecycleCumulativeCost: lifeCost,
		PaperRoundTripCostBPS:   e.cfg.PaperRoundTripCostBPS, USDTUSDCOneToOne: e.cfg.USDTUSDCOneToOne,
		OpenPositionCount: openCount, EntryCandidateCount: len(e.last.Single) + len(e.last.Cross),
		TypeSafeConfigured: e.typesafeEnabled(), SingleEntryEmptyReason: e.last.SingleEmptyReason,
		CrossEntryEmptyReason: e.last.CrossEmptyReason,
		StrategySingleStatus:  status, StrategyCrossStatus: status,
		SingleFilter: e.last.SingleStats, CrossFilter: e.last.CrossStats,
		DecisionJobs: e.lastModel.DecisionJobs, TypeSafeRequests: e.lastModel.Requests,
		TypeSafeSuccess: e.lastModel.Success, TypeSafeError: e.lastModel.Error,
		ModelStatus: e.lastModel.Status, LastModelCallAt: e.lastModel.LastModelCallAt,
		AvailableStrategyVersions: strategyVersions(runs),
	}
	published, err := e.store.PublishSnapshot(ctx, run.ID, snapshot)
	if err != nil {
		return err
	}
	if e.onSnap != nil {
		e.onSnap(published)
	}
	return nil
}

func canonicalPositionKeys(items []Position) []MarketKey {
	out := make([]MarketKey, 0, len(items)*2)
	for _, item := range items {
		if item.CanonicalOK() {
			out = append(out, item.LongKey(), item.ShortKey())
		}
	}
	return out
}

func candidateFromPosition(
	position Position,
	quotes map[string]Quote,
	rates []Rate,
	cfg config.Jev,
	now time.Time,
	lookup func(long, short MarketKey) (BasisForecast, bool),
) Candidate {
	candidate := Candidate{
		Key: position.CandidateKey, Kind: position.Kind, BaseAsset: position.BaseAsset,
		QuoteAsset: position.QuoteAsset, GlobalSymbol: position.GlobalSymbol,
		LongExchange: position.LongExchange, LongSymbol: position.LongSymbol, LongProduct: position.LongProduct,
		LongCanonicalSymbol: position.LongCanonicalSymbol, LongQuoteAsset: position.LongQuoteAsset,
		ShortExchange: position.ShortExchange, ShortSymbol: position.ShortSymbol, ShortProduct: position.ShortProduct,
		ShortCanonicalSymbol: position.ShortCanonicalSymbol, ShortQuoteAsset: position.ShortQuoteAsset,
		LongQuote: quotes[position.LongKey().QuoteKey()], ShortQuote: quotes[position.ShortKey().QuoteKey()],
	}
	for _, rate := range rates {
		if strings.EqualFold(rate.Exchange, position.ShortExchange) && strings.EqualFold(rate.ExchangeSymbol, position.ShortSymbol) {
			candidate.CurrentFundingShort = rate.FundingRate
			candidate.NextFundingShort = rate.NextFundingRate
			candidate.Cumulative24hShort = rate.Cumulative24h
		}
		if strings.EqualFold(rate.Exchange, position.LongExchange) && strings.EqualFold(rate.ExchangeSymbol, position.LongSymbol) &&
			position.LongProduct == ProductPerpetual {
			candidate.CurrentFundingLong = rate.FundingRate
			candidate.NextFundingLong = rate.NextFundingRate
			candidate.Cumulative24hLong = rate.Cumulative24h
		}
	}
	if position.Kind == KindSingle {
		candidate.FundingReturn24h = candidate.Cumulative24hShort
		candidate.Edge24h = candidate.Cumulative24hShort
	} else {
		candidate.Edge24h = candidate.Cumulative24hShort.Sub(candidate.Cumulative24hLong)
		candidate.FundingReturn24h = candidate.Edge24h
	}
	if lookup != nil {
		if forecast, ok := lookup(candidate.LongKey(), candidate.ShortKey()); ok {
			candidate = attachForecast(candidate, forecast)
		}
	}
	candidate = ApplyLocalReturns(cfg, candidate)
	_ = now
	return candidate
}

func indexPositions(items []Position) map[int64]Position {
	out := make(map[int64]Position, len(items))
	for _, item := range items {
		out[item.ID] = item
	}
	return out
}

func (e *Engine) insertDecision(ctx context.Context, decision Decision) (Decision, error) {
	if strings.TrimSpace(decision.StrategyVersion) == "" {
		decision.StrategyVersion = StrategyVersion
	}
	if len(decision.RawResponse) == 0 {
		decision.RawResponse = []byte(`{}`)
	}
	saved, err := e.store.InsertDecision(ctx, decision)
	if err != nil {
		return Decision{}, err
	}
	e.lastDecisionSaved.Store(true)
	return saved, nil
}

func (e *Engine) saveDecision(ctx context.Context, decision Decision) error {
	if err := e.store.SaveDecision(ctx, decision); err != nil {
		return err
	}
	e.lastDecisionSaved.Store(true)
	return nil
}

func clonePosition(position Position) *Position {
	copy := position
	return &copy
}

func ptrInt(value int64) *int64 { return &value }

func fundingReturnFromRates(position Position, rates []Rate) decimal.Decimal {
	var longCum, shortCum decimal.Decimal
	for _, rate := range rates {
		if strings.EqualFold(rate.Exchange, position.ShortExchange) && strings.EqualFold(rate.ExchangeSymbol, position.ShortSymbol) {
			shortCum = rate.Cumulative24h
		}
		if position.LongProduct == ProductPerpetual &&
			strings.EqualFold(rate.Exchange, position.LongExchange) && strings.EqualFold(rate.ExchangeSymbol, position.LongSymbol) {
			longCum = rate.Cumulative24h
		}
	}
	if position.Kind == KindSingle {
		return shortCum
	}
	return shortCum.Sub(longCum)
}

func (e *Engine) persistOtherRunTotals(ctx context.Context, currentRunID int64) error {
	runs, err := e.store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, item := range runs {
		if item.ID == currentRunID {
			continue
		}
		positions, err := e.store.ListAllPositions(ctx, item.ID)
		if err != nil {
			return err
		}
		item = RecomputeRunTotals(item, positions)
		if err := e.store.SaveRun(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func lifecycleFromRuns(runs []Run) (equity, net, funding, realized, unrealized, cost decimal.Decimal) {
	for _, run := range runs {
		equity = equity.Add(run.Equity)
		net = net.Add(run.NetPnL)
		funding = funding.Add(run.FundingPnL)
		realized = realized.Add(run.RealizedSpreadPnL)
		unrealized = unrealized.Add(run.UnrealizedSpreadPnL)
		cost = cost.Add(run.CumulativeCost)
	}
	return equity, net, funding, realized, unrealized, cost
}

func strategyVersions(runs []Run) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, run := range runs {
		version := strings.TrimSpace(run.StrategyVersion)
		if version == "" {
			continue
		}
		if _, ok := seen[version]; ok {
			continue
		}
		seen[version] = struct{}{}
		out = append(out, version)
	}
	sort.Strings(out)
	return out
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}
