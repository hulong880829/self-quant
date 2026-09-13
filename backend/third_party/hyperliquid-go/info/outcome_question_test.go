package info

import (
	"reflect"
	"testing"
)

// priceBucketQ matches the BTC-range-style market: two thresholds, three
// buckets, labels derived purely from the description.
func priceBucketQ() Question {
	return Question{
		Question:      1,
		Name:          "BTC price range on May 29",
		Description:   "class:priceBucket|underlying:BTC|priceThresholds:71492,74410",
		NamedOutcomes: []int{101, 102, 103},
	}
}

// categoricalQ matches the Champions League / Fed-rate-style market: no
// priceThresholds, label per bucket comes from the referenced
// OutcomeInfo.Name.
func categoricalQ() Question {
	return Question{
		Question:      2,
		Name:          "Champions League Winner",
		Description:   "class:enum|sport:football",
		NamedOutcomes: []int{201, 202},
	}
}

// mismatchedPriceBucketQ matches the CPI-style market: priceBucket class
// but a count mismatch — one threshold and three named outcomes (the
// venue added an "Exactly" bucket the thresholds-only label scheme can't
// describe).
func mismatchedPriceBucketQ() Question {
	return Question{
		Question:      3,
		Name:          "May CPI year-over-year",
		Description:   "class:priceBucket|underlying:CPI|priceThresholds:4.3",
		NamedOutcomes: []int{301, 302, 303},
	}
}

func fullMeta() *OutcomeMeta {
	pb, cat, cpi := priceBucketQ(), categoricalQ(), mismatchedPriceBucketQ()
	return &OutcomeMeta{
		Outcomes: []OutcomeInfo{
			{Outcome: 101, Name: "Below 71492"},
			{Outcome: 102, Name: "71492 to 74410"},
			{Outcome: 103, Name: "Above 74410"},
			{Outcome: 201, Name: "PSG"},
			{Outcome: 202, Name: "Arsenal"},
			{Outcome: 301, Name: "Below 4.3%"},
			{Outcome: 302, Name: "Exactly 4.3%"},
			{Outcome: 303, Name: "Above 4.3%"},
		},
		Questions: []Question{pb, cat, cpi},
	}
}

func TestQuestionLabels_PriceBucket(t *testing.T) {
	m := fullMeta()
	q := &m.Questions[0]
	want := []string{"Below 71492", "71492 to 74410", "Above 74410"}
	got := m.QuestionLabels(q)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priceBucket labels: got %v, want %v", got, want)
	}
}

func TestQuestionLabels_Categorical(t *testing.T) {
	m := fullMeta()
	q := &m.Questions[1]
	want := []string{"PSG", "Arsenal"}
	got := m.QuestionLabels(q)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("categorical labels: got %v, want %v", got, want)
	}
}

func TestQuestionLabels_PriceBucketCountMismatch_FallsBackToOutcomeNames(t *testing.T) {
	m := fullMeta()
	q := &m.Questions[2]
	// 1 threshold + 3 named outcomes is a mismatch; QuestionLabels should
	// resolve the names from OutcomeInfo instead of returning nil.
	want := []string{"Below 4.3%", "Exactly 4.3%", "Above 4.3%"}
	got := m.QuestionLabels(q)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CPI-style labels: got %v, want %v", got, want)
	}
}

func TestQuestionLabels_UnresolvableOutcomeReturnsNil(t *testing.T) {
	m := &OutcomeMeta{
		Outcomes: []OutcomeInfo{{Outcome: 201, Name: "PSG"}},
		Questions: []Question{{
			Name:          "Champions League Winner",
			NamedOutcomes: []int{201, 999}, // 999 is unknown
		}},
	}
	if got := m.QuestionLabels(&m.Questions[0]); got != nil {
		t.Fatalf("expected nil when an outcome name is missing, got %v", got)
	}
}

func TestQuestionLabels_NilSafety(t *testing.T) {
	var m *OutcomeMeta
	if got := m.QuestionLabels(&Question{NamedOutcomes: []int{1}}); got != nil {
		t.Fatalf("nil meta: got %v, want nil", got)
	}
	m2 := fullMeta()
	if got := m2.QuestionLabels(nil); got != nil {
		t.Fatalf("nil question: got %v, want nil", got)
	}
	if got := m2.QuestionLabels(&Question{}); got != nil {
		t.Fatalf("empty NamedOutcomes: got %v, want nil", got)
	}
}

func TestBucketLabel_PriceBucket(t *testing.T) {
	m := fullMeta()
	if got := m.BucketLabel(102); got != "71492 to 74410" {
		t.Fatalf("priceBucket inner bucket: got %q", got)
	}
	if got := m.BucketLabel(101); got != "Below 71492" {
		t.Fatalf("priceBucket first bucket: got %q", got)
	}
}

func TestBucketLabel_Categorical(t *testing.T) {
	m := fullMeta()
	if got := m.BucketLabel(201); got != "PSG" {
		t.Fatalf("categorical first bucket: got %q", got)
	}
	if got := m.BucketLabel(202); got != "Arsenal" {
		t.Fatalf("categorical second bucket: got %q", got)
	}
}

func TestBucketLabel_FallbackOutcome(t *testing.T) {
	m := &OutcomeMeta{
		Outcomes: []OutcomeInfo{{Outcome: 201, Name: "PSG"}, {Outcome: 202, Name: "Arsenal"}},
		Questions: []Question{{
			Name:            "Champions League Winner",
			Description:     "class:enum",
			NamedOutcomes:   []int{201, 202},
			FallbackOutcome: 999,
		}},
	}
	if got := m.BucketLabel(999); got != "Champions League Winner fallback" {
		t.Fatalf("fallback label: got %q", got)
	}
}

func TestBucketLabel_UnknownOutcomeReturnsEmpty(t *testing.T) {
	m := fullMeta()
	if got := m.BucketLabel(99999); got != "" {
		t.Fatalf("unknown outcome: got %q, want empty", got)
	}
}

func TestBucketLabel_OrphanOutcomeReturnsEmpty(t *testing.T) {
	// Outcome resolves through FindQuestion but the question's label list
	// is unbuildable (e.g. unresolvable sibling). Should return "".
	m := &OutcomeMeta{
		Outcomes: []OutcomeInfo{{Outcome: 201, Name: "PSG"}},
		Questions: []Question{{
			Name:          "Champions League Winner",
			NamedOutcomes: []int{201, 999},
		}},
	}
	if got := m.BucketLabel(201); got != "" {
		t.Fatalf("orphan question: got %q, want empty", got)
	}
}

func TestQuestionBuckets_PriceBucket(t *testing.T) {
	m := fullMeta()
	got := m.QuestionBuckets(&m.Questions[0])
	want := []Bucket{
		{Outcome: 101, Label: "Below 71492", YesCanonical: "#1010", NoCanonical: "#1011"},
		{Outcome: 102, Label: "71492 to 74410", YesCanonical: "#1020", NoCanonical: "#1021"},
		{Outcome: 103, Label: "Above 74410", YesCanonical: "#1030", NoCanonical: "#1031"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priceBucket buckets:\n got  %+v\n want %+v", got, want)
	}
}

func TestQuestionBuckets_Categorical(t *testing.T) {
	m := fullMeta()
	got := m.QuestionBuckets(&m.Questions[1])
	want := []Bucket{
		{Outcome: 201, Label: "PSG", YesCanonical: "#2010", NoCanonical: "#2011"},
		{Outcome: 202, Label: "Arsenal", YesCanonical: "#2020", NoCanonical: "#2021"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("categorical buckets:\n got  %+v\n want %+v", got, want)
	}
}

func TestQuestionBuckets_UnresolvableReturnsNil(t *testing.T) {
	m := &OutcomeMeta{
		Outcomes:  []OutcomeInfo{{Outcome: 201, Name: "PSG"}},
		Questions: []Question{{NamedOutcomes: []int{201, 999}}},
	}
	if got := m.QuestionBuckets(&m.Questions[0]); got != nil {
		t.Fatalf("unresolvable bucket list: got %v, want nil", got)
	}
}

// Question.BucketLabels stays priceBucket-pure; the consolidated
// fallback path lives on OutcomeMeta. Re-assert the existing contract
// so a future refactor doesn't quietly broaden it.
func TestQuestion_BucketLabels_PriceBucketOnly(t *testing.T) {
	if got := categoricalQ().BucketLabels(); got != nil {
		t.Fatalf("non-priceBucket: got %v, want nil", got)
	}
	if got := mismatchedPriceBucketQ().BucketLabels(); got != nil {
		t.Fatalf("priceBucket count mismatch: got %v, want nil", got)
	}
	got := priceBucketQ().BucketLabels()
	want := []string{"Below 71492", "71492 to 74410", "Above 74410"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priceBucket pure: got %v, want %v", got, want)
	}
}
