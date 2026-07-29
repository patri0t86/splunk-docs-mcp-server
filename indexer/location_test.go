package main

import (
	"reflect"
	"testing"
)

func TestDocLocation(t *testing.T) {
	cases := []struct {
		url, product, version   string
		wantManual, wantCanonID string
	}{
		{
			"https://help.splunk.com/en/splunk-enterprise/administer/admin-manual/10.4/configuration-file-reference/limitsconf",
			"splunk-enterprise", "10.4",
			"admin-manual", "splunk-enterprise/administer/admin-manual/configuration-file-reference/limitsconf",
		},
		// The same page in another release must land on the same canonical id,
		// which is what lets search collapse version copies.
		{
			"https://help.splunk.com/en/splunk-enterprise/administer/admin-manual/9.4/configuration-file-reference/limitsconf",
			"splunk-enterprise", "9.4",
			"admin-manual", "splunk-enterprise/administer/admin-manual/configuration-file-reference/limitsconf",
		},
		// Unversioned hosts have no version segment to drop.
		{
			"https://lantern.splunk.com/Splunk_Platform/Use_Cases/Security",
			"splunk-lantern", "",
			"Splunk_Platform", "splunk-lantern/splunk_platform/use_cases/security",
		},
		{"", "splunk-enterprise", "10.4", "", ""},
	}
	for _, c := range cases {
		manual, canonID := docLocation(c.url, c.product, c.version)
		if manual != c.wantManual || canonID != c.wantCanonID {
			t.Errorf("docLocation(%q) = (%q, %q), want (%q, %q)", c.url, manual, canonID, c.wantManual, c.wantCanonID)
		}
	}
}

func TestDocumentAliases(t *testing.T) {
	// These literals are the contract with the MCP server, which rebuilds the
	// same keys from a URL to resolve cross-references. If this changes, the
	// server's urlAliases must change with it or lookups silently stop
	// matching. Keep in sync with TestURLAliases in mcp-server.
	url := "https://help.splunk.com/en/splunk-enterprise/administer/admin-manual/10.4/configuration-file-reference/limitsconf"
	_, canonID := docLocation(url, "splunk-enterprise", "10.4")
	got := documentAliases(url, canonID)
	want := []string{
		"slug:limitsconf",
		"path:splunkenterpriseadministeradminmanualconfigurationfilereferencelimitsconf",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("documentAliases = %v, want %v", got, want)
	}
}

func TestNormalizeAlias(t *testing.T) {
	// A ?resourceId= topic id and the corresponding page slug must normalize
	// to the same key, since that is the only thing linking them.
	if normalizeAlias("Referencehardware") != normalizeAlias("reference-hardware") {
		t.Error("resourceId topic id and page slug should normalize equal")
	}
	if got := normalizeAlias("Splunk_Platform/Use_Cases"); got != "splunkplatformusecases" {
		t.Errorf("normalizeAlias = %q, want splunkplatformusecases", got)
	}
}

func TestFrontmatterSourceUpdated(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"2025-06-11T09:12:00Z", true},
		{"2025-06-11T09:12:00", true},
		{"2025-06-11", true},
		{"", false},
		{"null", false},
		{"not a date", false},
	}
	for _, c := range cases {
		fm := frontmatter{LastModified: c.in}
		if got := fm.sourceUpdated(); (got != nil) != c.want {
			t.Errorf("sourceUpdated(%q) non-nil = %v, want %v", c.in, got != nil, c.want)
		}
	}
}

func TestDocumentKeyIsStable(t *testing.T) {
	// section_id is derived from this key and is handed to clients as a
	// citable handle, so it must not change when a page is reindexed.
	a := &document{fm: frontmatter{URL: "https://help.splunk.com/en/a/b/c"}}
	b := &document{fm: frontmatter{URL: "https://help.splunk.com/en/a/b/c"}}
	c := &document{fm: frontmatter{URL: "https://help.splunk.com/en/a/b/d"}}
	if a.key() != b.key() {
		t.Error("key() should depend only on the URL")
	}
	if a.key() == c.key() {
		t.Error("different URLs should produce different keys")
	}
}
