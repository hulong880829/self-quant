package ranking

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"
)

const simulationPaths = 256

type PairPoint struct {
	TS    time.Time
	Long  Quote
	Short Quote
}

type PathOutcome struct {
	SpreadReturn float64
	ExitMinutes  float64
	Hit          bool
}

type ModelResult struct {
	CurrentMidSpreadBPS        float64
	CurrentExecutableSpreadBPS float64
	TargetSpreadBPS            float64
	FirstPassageProbability    float64
	ExpectedExitMinutes        float64
	Paths                      []PathOutcome
	Phi                        float64
	ResidualCount              int
}

func Analyze(period Period, points []PairPoint, seed int64) (ModelResult, error) {
	horizon := period.Horizon()
	lookback := period.Lookback()
	step := period.SimulationStep()
	if horizon <= 0 || lookback <= 0 || step <= 0 {
		return ModelResult{}, ErrInvalidPeriod
	}
	if len(points) < 120 {
		return ModelResult{}, fmt.Errorf("%w: fewer than 120 paired buckets", ErrInsufficient)
	}
	latest := points[len(points)-1]
	cutoff := latest.TS.Add(-lookback)
	start := sort.Search(len(points), func(index int) bool {
		return !points[index].TS.Before(cutoff)
	})
	points = points[start:]
	if len(points) < 120 {
		return ModelResult{}, fmt.Errorf("%w: lookback coverage", ErrInsufficient)
	}

	spreads := make([]float64, len(points))
	for index, point := range points {
		if point.Long.Mid() <= 0 || point.Short.Mid() <= 0 {
			return ModelResult{}, fmt.Errorf("%w: non-positive midpoint", ErrInsufficient)
		}
		spreads[index] = (point.Short.Mid()/point.Long.Mid() - 1) * 10_000
	}
	weights := recencyWeights(len(spreads), lookback)
	center := weightedMedian(spreads, weights)
	deviations := make([]float64, len(spreads))
	absolute := make([]float64, len(spreads))
	for index, value := range spreads {
		deviations[index] = value - center
		absolute[index] = math.Abs(deviations[index])
	}
	sigma := 1.4826 * weightedMedian(absolute, weights)
	if !finite(sigma) || sigma < 0.05 {
		return ModelResult{}, fmt.Errorf("%w: spread variance too low", ErrInsufficient)
	}
	phi := weightedAR1(deviations, weights)
	if !finite(phi) || phi >= 0.9999 {
		return ModelResult{}, fmt.Errorf("%w: non-mean-reverting spread", ErrInsufficient)
	}
	phi = math.Max(0, phi)
	residuals := make([]float64, 0, len(deviations)-1)
	for index := 1; index < len(deviations); index++ {
		residual := deviations[index] - phi*deviations[index-1]
		if finite(residual) {
			residuals = append(residuals, residual)
		}
	}
	if len(residuals) < 100 {
		return ModelResult{}, fmt.Errorf("%w: too few residuals", ErrInsufficient)
	}

	currentMid := spreads[len(spreads)-1]
	currentDeviation := currentMid - center
	targetBand := math.Max(1, 0.25*sigma)
	if math.Abs(currentDeviation) <= targetBand {
		return ModelResult{}, fmt.Errorf("%w: spread already in target band", ErrInsufficient)
	}
	currentExecutable := (latest.Short.Bid/latest.Long.Ask - 1) * 10_000
	if !finite(currentExecutable) {
		return ModelResult{}, fmt.Errorf("%w: invalid executable spread", ErrInsufficient)
	}
	longHalf := (latest.Long.Ask - latest.Long.Bid) / (2 * latest.Long.Mid())
	shortHalf := (latest.Short.Ask - latest.Short.Bid) / (2 * latest.Short.Mid())
	if longHalf < 0 || shortHalf < 0 {
		return ModelResult{}, fmt.Errorf("%w: crossed book", ErrInsufficient)
	}

	random := rand.New(rand.NewSource(seed))
	stepMinutes := max(1, int(step/time.Minute))
	totalMinutes := int(horizon / time.Minute)
	outcomes := make([]PathOutcome, 0, simulationPaths)
	hits := 0
	var exitMinuteSum float64
	for range simulationPaths {
		deviation := currentDeviation
		exitMinutes := float64(totalMinutes)
		hit := false
		for elapsed := stepMinutes; elapsed <= totalMinutes; elapsed += stepMinutes {
			blockStart := random.Intn(max(1, len(residuals)-stepMinutes+1))
			for minute := 0; minute < stepMinutes; minute++ {
				residual := residuals[(blockStart+minute)%len(residuals)]
				deviation = phi*deviation + residual
			}
			crossedTarget := (currentDeviation > 0 && deviation < 0) ||
				(currentDeviation < 0 && deviation > 0)
			if math.Abs(deviation) <= targetBand || crossedTarget {
				if crossedTarget {
					deviation = 0
				}
				hit = true
				exitMinutes = float64(elapsed)
				break
			}
		}
		exitMidSpread := center + deviation
		longMidExit := latest.Long.Mid()
		shortMidExit := longMidExit * (1 + exitMidSpread/10_000)
		longBidExit := longMidExit * (1 - longHalf)
		shortAskExit := shortMidExit * (1 + shortHalf)
		longReturn := longBidExit/latest.Long.Ask - 1
		shortReturn := 1 - shortAskExit/latest.Short.Bid
		spreadReturn := longReturn + shortReturn
		if !finite(spreadReturn) {
			continue
		}
		if hit {
			hits++
		}
		exitMinuteSum += exitMinutes
		outcomes = append(outcomes, PathOutcome{
			SpreadReturn: spreadReturn, ExitMinutes: exitMinutes, Hit: hit,
		})
	}
	if len(outcomes) < simulationPaths/2 {
		return ModelResult{}, fmt.Errorf("%w: invalid simulated paths", ErrInsufficient)
	}
	return ModelResult{
		CurrentMidSpreadBPS: currentMid, CurrentExecutableSpreadBPS: currentExecutable,
		TargetSpreadBPS: center, FirstPassageProbability: float64(hits) / float64(len(outcomes)),
		ExpectedExitMinutes: exitMinuteSum / float64(len(outcomes)), Paths: outcomes,
		Phi: phi, ResidualCount: len(residuals),
	}, nil
}

func recencyWeights(length int, lookback time.Duration) []float64 {
	result := make([]float64, length)
	halfLifeMinutes := math.Max(60, lookback.Minutes()/3)
	for index := range result {
		age := float64(length - 1 - index)
		result[index] = math.Exp(-math.Ln2 * age / halfLifeMinutes)
	}
	return result
}

func weightedMedian(values, weights []float64) float64 {
	type weightedValue struct{ value, weight float64 }
	items := make([]weightedValue, 0, len(values))
	var total float64
	for index, value := range values {
		weight := weights[index]
		if finite(value) && finite(weight) && weight > 0 {
			items = append(items, weightedValue{value: value, weight: weight})
			total += weight
		}
	}
	if len(items) == 0 {
		return math.NaN()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].value < items[j].value })
	var cumulative float64
	for _, item := range items {
		cumulative += item.weight
		if cumulative >= total/2 {
			return item.value
		}
	}
	return items[len(items)-1].value
}

func weightedAR1(values, weights []float64) float64 {
	var numerator, denominator float64
	for index := 1; index < len(values); index++ {
		weight := weights[index]
		numerator += weight * values[index-1] * values[index]
		denominator += weight * values[index-1] * values[index-1]
	}
	if denominator <= 0 {
		return math.NaN()
	}
	return numerator / denominator
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
