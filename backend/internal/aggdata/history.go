package aggdata

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type HistoryBucket struct {
	StartNS   uint64  `json:"start_ns,string"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	WindowMin float64 `json:"window_min"`
	WindowMax float64 `json:"window_max"`
	Samples   uint64  `json:"samples"`
	Coverage  float64 `json:"coverage"`
}

type HistoryGap struct {
	StartNS uint64 `json:"start_ns,string"`
	EndNS   uint64 `json:"end_ns,string"`
}

type HistoryResponse struct {
	Symbol            string          `json:"symbol"`
	Type              string          `json:"type"`
	StartNS           uint64          `json:"start_ns,string"`
	EndNS             uint64          `json:"end_ns,string"`
	ResolutionNS      uint64          `json:"resolution_ns,string"`
	Buckets           []HistoryBucket `json:"buckets"`
	Gaps              []HistoryGap    `json:"gaps"`
	Distribution      []float64       `json:"distribution_bps"`
	UnavailableShards []string        `json:"unavailable_shards,omitempty"`
	CurrentHourFromNS uint64          `json:"current_hour_from_ns,string,omitempty"`
}

type shardCache struct {
	records map[string][]RecordedBBO
}

type History struct {
	root      string
	workers   chan struct{}
	mu        sync.RWMutex
	cache     shardCache
	loadGroup singleflight.Group
}

func NewHistory(root string, workerCount int) *History {
	return &History{
		root: filepath.Clean(root), workers: make(chan struct{}, workerCount),
		cache: shardCache{records: make(map[string][]RecordedBBO)},
	}
}

func (h *History) Query(
	ctx context.Context,
	catalog *catalogSnapshot,
	market *liveMarket,
	resolution time.Duration,
	bpsType string,
	now time.Time,
) (HistoryResponse, error) {
	var response HistoryResponse
	if resolution <= 0 {
		return response, fmt.Errorf("resolution must be positive")
	}
	if bpsType != "raw" && bpsType != "gated" {
		return response, fmt.Errorf("type must be raw or gated")
	}
	end := now.UTC()
	start := end.Add(-24 * time.Hour)
	response = HistoryResponse{
		Symbol: market.catalog.Symbol, Type: bpsType,
		StartNS: uint64(start.UnixNano()), EndNS: uint64(end.UnixNano()),
		ResolutionNS: uint64(resolution),
	}
	h.pruneCache(catalog)
	var samples []LiveSample
	for _, relative := range market.catalog.Shards {
		if !isBBOShard(relative) {
			continue
		}
		records, err := h.readShard(ctx, relative)
		if err != nil {
			if ctx.Err() != nil {
				return response, ctx.Err()
			}
			response.UnavailableShards = append(response.UnavailableShards, relative)
			continue
		}
		for _, record := range records {
			if record.WallNS >= response.StartNS && record.WallNS <= response.EndNS {
				samples = append(samples, record.LiveSample)
			}
		}
	}
	live := market.ring.snapshot()
	if len(live) > 0 {
		response.CurrentHourFromNS = live[0].WallNS
	}
	for _, sample := range live {
		if sample.WallNS >= response.StartNS && sample.WallNS <= response.EndNS {
			if sample.WindowSamples == 0 {
				sample.WindowSamples = 1
				sample.RawMin, sample.RawMax = sample.RawBPS, sample.RawBPS
				sample.GatedMin, sample.GatedMax = sample.GatedBPS, sample.GatedBPS
			}
			samples = append(samples, sample)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].WallNS < samples[j].WallNS })
	samples = deduplicateSamples(samples)
	response.Buckets, response.Gaps, response.Distribution = aggregateHistory(
		samples, response.StartNS, response.EndNS, resolution,
		time.Duration(catalog.SampleIntervalMS)*time.Millisecond, bpsType,
	)
	return response, nil
}

func (h *History) pruneCache(catalog *catalogSnapshot) {
	keep := make(map[string]struct{})
	for _, market := range catalog.Markets {
		for _, shard := range market.Shards {
			if isBBOShard(shard) {
				keep[shard] = struct{}{}
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for shard := range h.cache.records {
		if _, exists := keep[shard]; !exists {
			delete(h.cache.records, shard)
		}
	}
}

func (h *History) readShard(ctx context.Context, relative string) ([]RecordedBBO, error) {
	h.mu.RLock()
	cached, exists := h.cache.records[relative]
	h.mu.RUnlock()
	if exists {
		return cached, nil
	}
	value, err, _ := h.loadGroup.Do(relative, func() (any, error) {
		select {
		case h.workers <- struct{}{}:
			defer func() { <-h.workers }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		var records []RecordedBBO
		full, err := h.secureShardPath(relative)
		if err != nil {
			return nil, err
		}
		_, err = ReadBBOShard(full, func(record RecordedBBO) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				records = append(records, record)
				return nil
			}
		})
		if err != nil {
			return nil, fmt.Errorf("read history shard %q: %w", relative, err)
		}
		h.mu.Lock()
		h.cache.records[relative] = records
		h.mu.Unlock()
		return records, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]RecordedBBO), nil
}

func (h *History) secureShardPath(relative string) (string, error) {
	full, err := filepath.EvalSymlinks(filepath.Join(h.root, relative))
	if err != nil {
		return "", fmt.Errorf("resolve history shard %q: %w", relative, err)
	}
	within, err := filepath.Rel(h.root, full)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("history shard %q escapes recording directory", relative)
	}
	return full, nil
}

func isBBOShard(path string) bool {
	return validShardFilename(filepath.Base(path), "aggbbo")
}

func deduplicateSamples(samples []LiveSample) []LiveSample {
	if len(samples) == 0 {
		return samples
	}
	output := samples[:0]
	for _, sample := range samples {
		if len(output) > 0 && output[len(output)-1].WallNS == sample.WallNS {
			output[len(output)-1] = sample
			continue
		}
		output = append(output, sample)
	}
	return output
}

func aggregateHistory(
	samples []LiveSample,
	startNS, endNS uint64,
	resolution, sampleInterval time.Duration,
	bpsType string,
) ([]HistoryBucket, []HistoryGap, []float64) {
	resolutionNS := uint64(resolution)
	firstBucket := startNS
	buckets := make(map[uint64]*HistoryBucket)
	var distribution []float64
	for _, sample := range samples {
		bucketStart := startNS + (sample.WallNS-startNS)/resolutionNS*resolutionNS
		value, minimum, maximum := sample.GatedBPS, sample.GatedMin, sample.GatedMax
		if bpsType == "raw" {
			value, minimum, maximum = sample.RawBPS, sample.RawMin, sample.RawMax
		}
		bucket := buckets[bucketStart]
		if bucket == nil {
			bucket = &HistoryBucket{
				StartNS: bucketStart, Open: value, High: value, Low: value, Close: value,
				WindowMin: minimum, WindowMax: maximum,
			}
			buckets[bucketStart] = bucket
		} else {
			bucket.High = max(bucket.High, value)
			bucket.Low = min(bucket.Low, value)
			bucket.Close = value
			bucket.WindowMin = min(bucket.WindowMin, minimum)
			bucket.WindowMax = max(bucket.WindowMax, maximum)
		}
		bucket.Samples++
		distribution = append(distribution, value)
	}
	var result []HistoryBucket
	var gaps []HistoryGap
	expected := float64(resolution) / float64(sampleInterval)
	for bucketStart := firstBucket; bucketStart < endNS; bucketStart += resolutionNS {
		if bucket := buckets[bucketStart]; bucket != nil {
			bucket.Coverage = min(1, float64(bucket.Samples)/expected)
			result = append(result, *bucket)
		} else if len(gaps) > 0 && gaps[len(gaps)-1].EndNS == bucketStart {
			gaps[len(gaps)-1].EndNS = min(endNS, bucketStart+resolutionNS)
		} else {
			gaps = append(gaps, HistoryGap{
				StartNS: bucketStart,
				EndNS:   min(endNS, bucketStart+resolutionNS),
			})
		}
		if ^uint64(0)-bucketStart < resolutionNS {
			break
		}
	}
	sort.Slice(distribution, func(i, j int) bool { return distribution[i] < distribution[j] })
	const maximumDistributionSamples = 10_000
	if len(distribution) > maximumDistributionSamples {
		sampled := make([]float64, maximumDistributionSamples)
		for index := range sampled {
			source := index * (len(distribution) - 1) / (maximumDistributionSamples - 1)
			sampled[index] = distribution[source]
		}
		distribution = sampled
	}
	return result, gaps, distribution
}
