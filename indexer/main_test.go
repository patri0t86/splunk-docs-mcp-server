package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestFilterLatestPatch(t *testing.T) {
	root := filepath.Join("data", "markdown", "splunk-enterprise")
	mk := func(version, name string) string {
		return filepath.Join(root, version, name)
	}
	paths := []string{
		mk("9.4", "about.md"),
		mk("9.4.0", "authorize-conf.md"),
		mk("9.4.13", "authorize-conf.md"),
		mk("9.4.2", "authorize-conf.md"),
		mk("10.4", "about.md"),
		mk("10.4.0", "authorize-conf.md"),
		mk("10.4.1", "authorize-conf.md"),
		filepath.Join(root, "no-version", "page.md"),
	}
	want := []string{
		mk("9.4", "about.md"),
		mk("9.4.13", "authorize-conf.md"),
		mk("10.4", "about.md"),
		mk("10.4.1", "authorize-conf.md"),
		filepath.Join(root, "no-version", "page.md"),
	}
	got := filterLatestPatch(root, append([]string(nil), paths...))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterLatestPatch:\n got %v\nwant %v", got, want)
	}
}

func TestFilterLatestPatchMultiProduct(t *testing.T) {
	root := filepath.Join("data", "markdown")
	mk := func(product, version, name string) string {
		return filepath.Join(root, product, version, name)
	}
	paths := []string{
		mk("splunk-enterprise", "10.4", "about.md"),
		mk("splunk-enterprise", "10.4.0", "authorize-conf.md"),
		mk("splunk-enterprise", "10.4.1", "authorize-conf.md"),
		mk("splunk-cloud-platform", "10.4.2604", "service-details.md"),
		mk("splunk-soar", "6.3.0", "page.md"),
		mk("splunk-soar", "6.3.1", "page.md"),
	}
	want := []string{
		mk("splunk-enterprise", "10.4", "about.md"),
		mk("splunk-enterprise", "10.4.1", "authorize-conf.md"),
		mk("splunk-cloud-platform", "10.4.2604", "service-details.md"),
		mk("splunk-soar", "6.3.1", "page.md"),
	}
	got := filterLatestPatch(root, append([]string(nil), paths...))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterLatestPatch:\n got %v\nwant %v", got, want)
	}
}

func TestSplitChunks(t *testing.T) {
	body := "# Page Title\n\nIntro paragraph that must not be lost.\n\n" +
		"## Configure\n\ntext before code\n\n```conf\n## not a heading\n### also not\n```\n\nafter code\n\n" +
		"### Details\n\nnested section\n"
	got := splitChunks(body)
	want := []chunk{
		{heading: "", level: 0, content: "Intro paragraph that must not be lost."},
		{heading: "Configure", level: 2, content: "text before code\n\n```conf\n## not a heading\n### also not\n```\n\nafter code"},
		{heading: "Details", level: 3, content: "nested section"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitChunks:\n got %+v\nwant %+v", got, want)
	}

	// No headings: whole body (minus the h1) is one heading-less chunk.
	got = splitChunks("# Title\n\nJust a short page.\n")
	want = []chunk{{content: "Just a short page."}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitChunks no-headings:\n got %+v\nwant %+v", got, want)
	}
}

func TestURLProduct(t *testing.T) {
	cases := map[string]string{
		"https://help.splunk.com/en/splunk-enterprise/get-started/10.4/about": "splunk-enterprise",
		"https://help.splunk.com/en/splunk-cloud-platform/search/10.5.2605/x": "splunk-cloud-platform",
		"https://example.com/whatever":                                        "",
	}
	for url, want := range cases {
		if got := urlProduct(url); got != want {
			t.Errorf("urlProduct(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestPathVersion(t *testing.T) {
	root := "/data/markdown/splunk-enterprise"
	cases := []struct {
		path string
		want []int
	}{
		{root + "/10.4/admin/page.md", []int{10, 4}},
		{root + "/9.4.13/conf/page.md", []int{9, 4, 13}},
		{root + "/notes/page.md", nil},
	}
	for _, c := range cases {
		if got := pathVersion(root, c.path); !reflect.DeepEqual(got, c.want) {
			t.Errorf("pathVersion(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestEmbedCache(t *testing.T) {
	c := newEmbedCache()
	if _, ok := c.get("k"); ok {
		t.Fatal("get on empty cache returned ok")
	}
	vec := []float32{1, 2, 3}
	c.put("k", vec)
	got, ok := c.get("k")
	if !ok || !reflect.DeepEqual(got, vec) {
		t.Errorf("get(k) = %v, %v", got, ok)
	}
	hits, unique := c.stats()
	if hits != 1 || unique != 1 {
		t.Errorf("stats = %d hits, %d unique; want 1, 1", hits, unique)
	}
}

func TestParseFrontmatterBreadcrumbs(t *testing.T) {
	raw := []byte(`---
url: https://example.com/page
title: Reference hardware
version: '10.4'
breadcrumbs:
- Splunk Enterprise
- Get Started
- Reference hardware
---
body text`)
	fm, body, err := parseFrontmatter(raw)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if body != "body text" {
		t.Errorf("body = %q", body)
	}
	want := "Splunk Enterprise > Get Started > Reference hardware"
	if got := fm.breadcrumb(); got != want {
		t.Errorf("breadcrumb() = %q, want %q", got, want)
	}
}

func TestEmbedInput(t *testing.T) {
	doc := &document{
		fm: frontmatter{Title: "Reference hardware"},
		chunks: []chunk{
			{heading: "Mid-range indexer specification", content: "64 GB RAM."},
			{heading: "", content: "Intro text."},
		},
	}
	want0 := "search_document: Reference hardware > Mid-range indexer specification\n64 GB RAM."
	if got := doc.embedInput(0); got != want0 {
		t.Errorf("embedInput(0) = %q, want %q", got, want0)
	}
	want1 := "search_document: Reference hardware\nIntro text."
	if got := doc.embedInput(1); got != want1 {
		t.Errorf("embedInput(1) = %q, want %q", got, want1)
	}
}
