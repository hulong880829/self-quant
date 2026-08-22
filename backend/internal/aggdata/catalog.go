package aggdata

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var ErrManifestMissing = errors.New("recording manifest is missing")

type catalogMarket struct {
	Identity
	Segments  map[Kind]string
	Shards    []string
	Recording bool
}

type catalogSnapshot struct {
	State            string
	Hash             [32]byte
	SampleIntervalMS uint64
	RetentionHours   uint32
	Depth            uint32
	Markets          []catalogMarket
}

type Catalog struct {
	root    string
	current atomic.Pointer[catalogSnapshot]
}

type manifestDocument struct {
	Version          int    `json:"version"`
	State            string `json:"state"`
	SampleIntervalMS uint64 `json:"sample_interval_ms"`
	RetentionHours   uint32 `json:"retention_hours"`
	Depth            uint32 `json:"depth"`
	Active           []struct {
		Segment string `json:"segment"`
		Kind    string `json:"kind"`
	} `json:"active"`
	Shards []string `json:"shards"`
}

func NewCatalog(root string) (*Catalog, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve recording directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat recording directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("recording path is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve recording directory symlinks: %w", err)
	}
	return &Catalog{root: filepath.Clean(resolved)}, nil
}

func (c *Catalog) Snapshot() *catalogSnapshot {
	return c.current.Load()
}

func (c *Catalog) Root() string {
	return c.root
}

func (c *Catalog) Refresh() (bool, error) {
	path := filepath.Join(c.root, "manifest.v1.json")
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, ErrManifestMissing
	}
	if err != nil {
		return false, fmt.Errorf("read manifest: %w", err)
	}
	hash := sha256.Sum256(body)
	if previous := c.current.Load(); previous != nil && previous.Hash == hash {
		return false, nil
	}
	parsed, err := c.parseManifest(body, hash)
	if err != nil {
		return false, err
	}
	c.current.Store(parsed)
	return true, nil
}

func (c *Catalog) Watch(ctx context.Context, interval time.Duration, changed chan<- struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		updated, _ := c.Refresh()
		if updated && changed != nil {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Catalog) parseManifest(body []byte, hash [32]byte) (*catalogSnapshot, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var document manifestDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("manifest contains trailing data")
	}
	if document.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version %d", document.Version)
	}
	if document.SampleIntervalMS < 200 || document.RetentionHours == 0 ||
		document.RetentionHours > 24 || document.Depth == 0 || document.Depth > maxDepth {
		return nil, fmt.Errorf("invalid manifest recording bounds")
	}
	switch document.State {
	case "starting", "recording", "stopped", "failed":
	default:
		return nil, fmt.Errorf("invalid manifest state %q", document.State)
	}
	if document.State != "recording" && len(document.Active) != 0 {
		return nil, fmt.Errorf("manifest active topics require recording state")
	}
	byKey := make(map[string]*catalogMarket)
	for _, active := range document.Active {
		identity, err := parseSegment(active.Segment)
		if err != nil {
			return nil, fmt.Errorf("active segment %q: %w", active.Segment, err)
		}
		if active.Kind != identity.Stream {
			return nil, fmt.Errorf("active kind does not match segment")
		}
		kind := streamKind(identity.Stream)
		entry := ensureCatalogMarket(byKey, identity)
		if _, exists := entry.Segments[kind]; exists {
			return nil, fmt.Errorf("duplicate active segment for %s/%s", identity.Profile, identity.Symbol)
		}
		entry.Segments[kind] = active.Segment
		entry.Recording = document.State == "recording"
	}
	for _, shard := range document.Shards {
		identity, clean, err := c.parseShard(shard)
		if err != nil {
			return nil, fmt.Errorf("manifest shard %q: %w", shard, err)
		}
		entry := ensureCatalogMarket(byKey, identity)
		entry.Shards = append(entry.Shards, clean)
	}
	result := &catalogSnapshot{
		State: document.State, Hash: hash,
		SampleIntervalMS: document.SampleIntervalMS,
		RetentionHours:   document.RetentionHours,
		Depth:            document.Depth,
	}
	for _, entry := range byKey {
		sort.Strings(entry.Shards)
		result.Markets = append(result.Markets, *entry)
	}
	sort.Slice(result.Markets, func(i, j int) bool {
		if result.Markets[i].Symbol == result.Markets[j].Symbol {
			return result.Markets[i].Profile < result.Markets[j].Profile
		}
		return result.Markets[i].Symbol < result.Markets[j].Symbol
	})
	return result, nil
}

func ensureCatalogMarket(markets map[string]*catalogMarket, identity Identity) *catalogMarket {
	key := identity.key()
	entry := markets[key]
	if entry == nil {
		entry = &catalogMarket{
			Identity: Identity{Profile: identity.Profile, Symbol: identity.Symbol},
			Segments: make(map[Kind]string),
		}
		markets[key] = entry
	}
	return entry
}

func parseSegment(segment string) (Identity, error) {
	var identity Identity
	for _, stream := range []string{"aggbbo", "aggorderbook"} {
		suffix := "." + stream + ".2"
		if !strings.HasSuffix(segment, suffix) {
			continue
		}
		prefix := strings.TrimSuffix(segment, suffix)
		symbolDot := strings.LastIndexByte(prefix, '.')
		if !strings.HasPrefix(segment, "/") || symbolDot <= 1 || symbolDot == len(prefix)-1 {
			break
		}
		profileDot := strings.LastIndexByte(prefix[:symbolDot], '.')
		if profileDot < 1 || profileDot == symbolDot-1 {
			break
		}
		identity = Identity{
			Profile: prefix[profileDot+1 : symbolDot],
			Symbol:  strings.ToUpper(prefix[symbolDot+1:]),
			Stream:  stream,
		}
		if validComponent(identity.Profile) && validComponent(identity.Symbol) {
			return identity, nil
		}
		break
	}
	return Identity{}, fmt.Errorf("invalid aggregate segment")
}

func (c *Catalog) parseShard(shard string) (Identity, string, error) {
	var identity Identity
	if shard == "" || filepath.IsAbs(shard) || filepath.Clean(shard) != filepath.FromSlash(shard) {
		return identity, "", fmt.Errorf("shard must be a clean relative path")
	}
	parts := strings.Split(filepath.ToSlash(shard), "/")
	if len(parts) != 5 || len(parts[0]) != 10 || len(parts[1]) != 2 ||
		!validComponent(parts[2]) || !validComponent(parts[3]) {
		return identity, "", fmt.Errorf("invalid shard layout")
	}
	if _, err := time.Parse("2006-01-02/15", parts[0]+"/"+parts[1]); err != nil {
		return identity, "", fmt.Errorf("invalid shard hour")
	}
	stream := ""
	switch {
	case validShardFilename(parts[4], "aggbbo"):
		stream = "aggbbo"
	case validShardFilename(parts[4], "aggorderbook"):
		stream = "aggorderbook"
	default:
		return identity, "", fmt.Errorf("unsupported shard stream")
	}
	if !strings.HasSuffix(parts[4], ".sqrec.zst") {
		return identity, "", fmt.Errorf("invalid shard filename")
	}
	clean := filepath.Clean(filepath.FromSlash(shard))
	full := filepath.Join(c.root, clean)
	relative, err := filepath.Rel(c.root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return identity, "", fmt.Errorf("shard escapes recording directory")
	}
	identity = Identity{
		Profile: parts[2],
		Symbol:  strings.ToUpper(parts[3]),
		Stream:  stream,
	}
	return identity, clean, nil
}

func validShardFilename(name, stream string) bool {
	if name == stream+".sqrec.zst" {
		return true
	}
	prefix, suffix := stream+".", ".sqrec.zst"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	timestamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if timestamp == "" {
		return false
	}
	for _, character := range timestamp {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validComponent(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func streamKind(stream string) Kind {
	if stream == "aggbbo" {
		return KindBBO
	}
	return KindBook
}
