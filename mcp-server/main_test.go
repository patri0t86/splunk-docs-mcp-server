package main

import "testing"

func TestExtractSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://help.splunk.com/?resourceId=Splunk_Capacity_Referencehardware", "referencehardware"},
		{"https://help.splunk.com/en?resourceId=Splunk_Indexer_Pipelinesets", "pipelinesets"},
		{"https://help.splunk.com/en/splunk-enterprise/get-started/deployment-capacity-manual/10.4/performance-reference/reference-hardware", "referencehardware"},
		{"https://help.splunk.com/en/some/path/page#anchor", "page"},
		{"https://help.splunk.com/", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractSlug(c.in); got != c.want {
			t.Errorf("extractSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOrQuery(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"mid-range indexer hardware", "mid-range OR indexer OR hardware"},
		{"indexer", "indexer"},
		{"", ""},
	}
	for _, c := range cases {
		if got := orQuery(c.in); got != c.want {
			t.Errorf("orQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
