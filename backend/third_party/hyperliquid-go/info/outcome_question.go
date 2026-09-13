package info

import (
	"fmt"
	"strings"
)

// ParseOutcomeDescription splits a HIP-4 description string into its
// constituent key/value pairs. Hyperliquid encodes outcome and question
// metadata as "class:priceBinary|underlying:BTC|expiry:..." — a flat
// pipe-delimited map. Unknown shapes return an empty map; the function
// never panics on malformed input.
func ParseOutcomeDescription(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, kv := range strings.Split(s, "|") {
		idx := strings.Index(kv, ":")
		if idx <= 0 || idx == len(kv)-1 {
			continue
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out
}

// BucketLabels derives labels purely from q.Description, for the
// class:priceBucket case where thresholds [T1, T2, ..., Tn] map onto
// n+1 named outcomes as ["Below T1", "T1 to T2", ..., "Above Tn"].
//
// Returns nil for any other shape: a different class (categorical
// markets like "PSG / Arsenal"), missing thresholds, or a count
// mismatch (e.g. a "priceBucket" question with an extra "Exactly"
// bucket the thresholds-only scheme can't describe). For the general
// "give me labels for this question" case, prefer
// OutcomeMeta.QuestionLabels, which falls back to OutcomeInfo.Name
// lookup when the threshold derivation can't fire.
func (q Question) BucketLabels() []string {
	return bucketLabelsFromThresholds(q)
}

// bucketLabelsFromThresholds is the shared priceBucket label derivation
// used by both Question.BucketLabels and OutcomeMeta.QuestionLabels.
func bucketLabelsFromThresholds(q Question) []string {
	desc := ParseOutcomeDescription(q.Description)
	if desc["class"] != "priceBucket" {
		return nil
	}
	raw := desc["priceThresholds"]
	if raw == "" {
		return nil
	}
	thresholds := strings.Split(raw, ",")
	if len(q.NamedOutcomes) != len(thresholds)+1 {
		return nil
	}
	labels := make([]string, len(q.NamedOutcomes))
	labels[0] = "Below " + thresholds[0]
	for i := 1; i < len(thresholds); i++ {
		labels[i] = thresholds[i-1] + " to " + thresholds[i]
	}
	labels[len(thresholds)] = "Above " + thresholds[len(thresholds)-1]
	return labels
}

// QuestionLabels returns one human-readable label per entry in
// q.NamedOutcomes. For class:priceBucket questions whose threshold
// count matches (n thresholds → n+1 buckets) the labels are derived as
// "Below T1", "T1 to T2", …, "Above Tn". For every other shape —
// categorical markets, or priceBucket questions whose named-outcome
// count doesn't match the threshold count — each label falls back to
// the OutcomeInfo.Name of the referenced outcome.
//
// Returns nil when m or q is nil, q has no named outcomes, or any
// named outcome cannot be resolved to a name through m.Outcomes (so
// callers never receive a partial list).
func (m *OutcomeMeta) QuestionLabels(q *Question) []string {
	if m == nil || q == nil || len(q.NamedOutcomes) == 0 {
		return nil
	}
	if labels := bucketLabelsFromThresholds(*q); labels != nil {
		return labels
	}
	return m.namesForOutcomes(q.NamedOutcomes)
}

// namesForOutcomes resolves each outcome id to its OutcomeInfo.Name.
// Returns nil when any id is missing from m.Outcomes — partial label
// lists would mislead callers (a bucket without a label is unlabelable,
// not "unnamed").
func (m *OutcomeMeta) namesForOutcomes(outcomes []int) []string {
	index := make(map[int]string, len(m.Outcomes))
	for _, oc := range m.Outcomes {
		index[oc.Outcome] = oc.Name
	}
	names := make([]string, len(outcomes))
	for i, o := range outcomes {
		n := index[o]
		if n == "" {
			return nil
		}
		names[i] = n
	}
	return names
}

// FindQuestion locates the Question that owns outcome by walking
// NamedOutcomes and FallbackOutcome. Returns nil when no question
// references the outcome (binary markets like a class:priceBinary
// outcome stand alone).
func (m *OutcomeMeta) FindQuestion(outcome int) *Question {
	if m == nil {
		return nil
	}
	for i := range m.Questions {
		q := &m.Questions[i]
		if q.FallbackOutcome == outcome {
			return q
		}
		for _, o := range q.NamedOutcomes {
			if o == outcome {
				return q
			}
		}
	}
	return nil
}

// BucketLabel returns the human-readable bucket name for outcome
// within its parent question, or the empty string when outcome is
// not a known bucket. priceBucket questions get threshold-derived
// labels; everything else falls back to the OutcomeInfo.Name, so
// categorical markets like "PSG / Arsenal" are covered too.
func (m *OutcomeMeta) BucketLabel(outcome int) string {
	q := m.FindQuestion(outcome)
	if q == nil {
		return ""
	}
	if outcome == q.FallbackOutcome {
		return q.Name + " fallback"
	}
	labels := m.QuestionLabels(q)
	if labels == nil {
		return ""
	}
	for i, o := range q.NamedOutcomes {
		if o == outcome {
			return labels[i]
		}
	}
	return ""
}

// QuestionByName returns the first Question whose Name matches.
func (m *OutcomeMeta) QuestionByName(name string) *Question {
	if m == nil {
		return nil
	}
	for i := range m.Questions {
		if m.Questions[i].Name == name {
			return &m.Questions[i]
		}
	}
	return nil
}

// Bucket pairs a tradable HIP-4 outcome with its human label. The
// canonical names (`#<10*outcome+sideIdx>`) are what PlaceMarket /
// PlaceMany expect; the Label is for display.
type Bucket struct {
	Outcome      int    // outcome id within the bucket
	Label        string // e.g. "75348 to 78423" (priceBucket) or "PSG" (categorical)
	YesCanonical string // "#<10*outcome+0>"
	NoCanonical  string // "#<10*outcome+1>"
}

// Buckets returns one Bucket per entry in q.NamedOutcomes for the
// priceBucket case. Returns nil for non-priceBucket questions or
// threshold mismatches — use OutcomeMeta.QuestionBuckets when you want
// categorical markets covered too.
func (q Question) Buckets() []Bucket {
	labels := bucketLabelsFromThresholds(q)
	if labels == nil {
		return nil
	}
	return makeBuckets(q.NamedOutcomes, labels)
}

// QuestionBuckets returns one Bucket per entry in q.NamedOutcomes,
// using QuestionLabels for label derivation (priceBucket-aware with a
// categorical fallback). Returns nil when QuestionLabels does — i.e.
// when at least one bucket can't be labelled.
func (m *OutcomeMeta) QuestionBuckets(q *Question) []Bucket {
	labels := m.QuestionLabels(q)
	if labels == nil {
		return nil
	}
	return makeBuckets(q.NamedOutcomes, labels)
}

// makeBuckets zips named outcomes with their labels into Bucket rows.
// Caller guarantees len(outcomes) == len(labels).
func makeBuckets(outcomes []int, labels []string) []Bucket {
	out := make([]Bucket, len(outcomes))
	for i, o := range outcomes {
		out[i] = Bucket{
			Outcome:      o,
			Label:        labels[i],
			YesCanonical: fmt.Sprintf("#%d", 10*o+0),
			NoCanonical:  fmt.Sprintf("#%d", 10*o+1),
		}
	}
	return out
}
