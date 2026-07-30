package main

import (
	"slices"
	"testing"
)

func TestIdentifierTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// Dotted and underscored settings names are the payload of an exact search.
		{"what does max_mem_usage_mb do", []string{"max_mem_usage_mb"}},
		{"set limits.conf maxKBps", []string{"limits.conf"}},
		{"props.conf TRANSFORMS-null stanza", []string{"props.conf", "TRANSFORMS-null"}},
		{"index::main filtering", []string{"index::main"}},
		{"/services/data/indexes endpoint", []string{"/services/data/indexes"}},
		// SPL command names are identifiers even though they are bare words.
		{"how do I use tstats and stats", []string{"tstats", "stats"}},
		// Ordinary prose carries no identifiers, so search stays conceptual.
		{"how does indexer clustering work", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := identifierTokens(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("identifierTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLegWeights(t *testing.T) {
	// exact mode must never let the semantic leg outrank the literal one, and
	// conceptual mode must switch the literal leg off entirely.
	_, _, exactOnly, semanticInExact := legWeights(modeExact, false)
	if exactOnly <= semanticInExact {
		t.Errorf("exact mode: identifier weight %v should exceed semantic weight %v", exactOnly, semanticInExact)
	}
	_, _, exactInConceptual, semanticOnly := legWeights(modeConceptual, true)
	if exactInConceptual != 0 {
		t.Errorf("conceptual mode: identifier weight = %v, want 0", exactInConceptual)
	}
	if semanticOnly <= 0 {
		t.Errorf("conceptual mode: semantic weight = %v, want > 0", semanticOnly)
	}
	// auto only turns the literal leg on when the query actually contains one.
	_, _, exactWithIDs, _ := legWeights(modeAuto, true)
	_, _, exactWithoutIDs, _ := legWeights(modeAuto, false)
	if exactWithIDs <= exactWithoutIDs {
		t.Errorf("auto mode: identifier weight with ids %v should exceed without ids %v", exactWithIDs, exactWithoutIDs)
	}
}

func TestMergeFilter(t *testing.T) {
	// pgx encodes a nil slice as SQL NULL, and cardinality(NULL) is NULL, which
	// would silently filter every row out. The result must never be nil.
	if got := mergeFilter("", nil); got == nil || len(got) != 0 {
		t.Errorf("mergeFilter(\"\", nil) = %#v, want empty non-nil slice", got)
	}
	got := mergeFilter("splunk-enterprise", []string{"splunk-cloud-platform", "splunk-enterprise", ""})
	want := []string{"splunk-cloud-platform", "splunk-enterprise"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("mergeFilter merged = %v, want %v", got, want)
	}
}

func TestUnifiedDiff(t *testing.T) {
	got := unifiedDiff("alpha\nbeta\ngamma\n", "alpha\ndelta\ngamma\n", 1)
	lines := splitDiffLines(got)
	var removed, added bool
	for _, line := range lines {
		if line == "-beta" {
			removed = true
		}
		if line == "+delta" {
			added = true
		}
	}
	if !removed || !added {
		t.Errorf("unifiedDiff = %q, want -beta and +delta", got)
	}
	if unifiedDiff("same\n", "same\n", 1) != "" {
		t.Error("unifiedDiff on identical text should be empty")
	}
}

func TestURLAliases(t *testing.T) {
	// The path alias has to reproduce the indexer's canonical_id byte for byte,
	// product slug included, or the exact lookup never matches. This literal is
	// pinned by TestDocumentAliases in the indexer module too.
	got := urlAliases("https://help.splunk.com/en/splunk-enterprise/administer/admin-manual/10.4/configuration-file-reference/limitsconf")
	want := []string{
		"path:splunkenterpriseadministeradminmanualconfigurationfilereferencelimitsconf",
		"slug:limitsconf",
	}
	if !slices.Equal(got, want) {
		t.Errorf("urlAliases = %v, want %v", got, want)
	}
	// A resourceId URL carries no usable path, so only the slug alias applies.
	if got := urlAliases("https://help.splunk.com/?resourceId=Splunk_Indexer_Pipelinesets"); !slices.Equal(got, []string{"slug:pipelinesets"}) {
		t.Errorf("urlAliases(resourceId) = %v, want [slug:pipelinesets]", got)
	}
}
