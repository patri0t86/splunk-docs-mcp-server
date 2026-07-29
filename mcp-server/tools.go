package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxSectionChars bounds how much section text one call may return, so a
// citation lookup can't blow the caller's context the way a whole reference
// page can.
const maxSectionChars = 12000

// maxContextSections bounds how many neighbouring sections may be pulled in
// around the requested one.
const maxContextSections = 5

// maxIDLen bounds the identifiers callers hand back to the server.
const maxIDLen = 512

type GetSectionArgs struct {
	SectionID     string `json:"section_id" jsonschema:"the section_id from a search_docs result"`
	ContextBefore int    `json:"context_before,omitempty" jsonschema:"number of preceding sections to include for context, default 0"`
	ContextAfter  int    `json:"context_after,omitempty" jsonschema:"number of following sections to include for context, default 0"`
	MaxChars      int    `json:"max_chars,omitempty" jsonschema:"truncate the returned text at this many characters, default 12000"`
}

type NeighborSection struct {
	SectionID string `json:"section_id"`
	Heading   string `json:"heading"`
	Content   string `json:"content"`
}

type SectionOutput struct {
	SectionID       string            `json:"section_id"`
	DocumentID      string            `json:"document_id" jsonschema:"version-independent identifier of the page"`
	Title           string            `json:"title"`
	Product         string            `json:"product"`
	Version         string            `json:"version"`
	Manual          string            `json:"manual"`
	Breadcrumb      []string          `json:"breadcrumb"`
	Heading         string            `json:"heading"`
	Anchor          string            `json:"anchor"`
	URL             string            `json:"url"`
	CitationURL     string            `json:"citation_url" jsonschema:"url with the section anchor applied, cite this"`
	Content         string            `json:"content"`
	Truncated       bool              `json:"truncated" jsonschema:"true when content was cut at max_chars"`
	ContextBefore   []NeighborSection `json:"context_before"`
	ContextAfter    []NeighborSection `json:"context_after"`
	ContentHash     string            `json:"content_hash"`
	SourceUpdatedAt string            `json:"source_updated_at,omitempty"`
}

// getSection returns one section of a page, bounded and citation-ready.
// get_page is authoritative but reassembles every chunk, which wastes context
// on long reference pages and makes citations imprecise; this is the normal
// follow-up to search_docs, with get_page reserved for broad reading.
func getSection(ctx context.Context, req *mcp.CallToolRequest, args GetSectionArgs) (*mcp.CallToolResult, *SectionOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	sectionID := strings.TrimSpace(args.SectionID)
	if sectionID == "" {
		return nil, nil, errors.New("section_id must not be empty")
	}
	if len(sectionID) > maxIDLen {
		return nil, nil, fmt.Errorf("section_id too long (max %d bytes)", maxIDLen)
	}
	before := min(max(args.ContextBefore, 0), maxContextSections)
	after := min(max(args.ContextAfter, 0), maxContextSections)
	maxChars := args.MaxChars
	if maxChars <= 0 || maxChars > maxSectionChars {
		maxChars = maxSectionChars
	}
	log.Printf("get_section: section_id=%q before=%d after=%d max_chars=%d", sectionID, before, after, maxChars)

	release, err := acquireToolSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	start := time.Now()
	var out SectionOutput
	var docID, chunkIndex int
	var breadcrumb, anchor *string
	var sourceUpdated *time.Time
	err = pool.QueryRow(ctx, `
		SELECT c.document_id, c.chunk_index, c.anchor, c.heading, c.content,
		       d.canonical_id, d.title, d.product, d.version, d.manual,
		       d.breadcrumb, d.url, d.content_hash, d.source_updated_at
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.section_id = $1
		LIMIT 1
	`, sectionID).Scan(&docID, &chunkIndex, &anchor, &out.Heading, &out.Content,
		&out.DocumentID, &out.Title, &out.Product, &out.Version, &out.Manual,
		&breadcrumb, &out.URL, &out.ContentHash, &sourceUpdated)
	if errors.Is(err, pgx.ErrNoRows) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: "No section found for that section_id. Section ids come from search_docs results; use get_page if you only have a URL.",
		}}}, nil, nil
	}
	if err != nil {
		log.Printf("get_section: ERROR after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return nil, nil, errors.New("section fetch failed, try again shortly")
	}

	out.SectionID = sectionID
	if anchor != nil {
		out.Anchor = *anchor
	}
	if breadcrumb != nil && *breadcrumb != "" {
		out.Breadcrumb = strings.Split(*breadcrumb, " > ")
	}
	if sourceUpdated != nil {
		out.SourceUpdatedAt = sourceUpdated.UTC().Format(time.RFC3339)
	}
	out.CitationURL = out.URL
	if out.Anchor != "" && out.Anchor != "overview" {
		out.CitationURL = out.URL + "#" + out.Anchor
	}
	if len(out.Content) > maxChars {
		out.Content = out.Content[:maxChars]
		out.Truncated = true
	}
	out.ContextBefore = []NeighborSection{}
	out.ContextAfter = []NeighborSection{}

	if before > 0 || after > 0 {
		rows, err := pool.Query(ctx, `
			SELECT chunk_index, section_id, heading, content
			FROM chunks
			WHERE document_id = $1
			  AND chunk_index BETWEEN $2 - $3 AND $2 + $4
			  AND chunk_index <> $2
			ORDER BY chunk_index
		`, docID, chunkIndex, before, after)
		if err != nil {
			log.Printf("get_section: ERROR fetching context: %v", err)
			return nil, nil, errors.New("section fetch failed, try again shortly")
		}
		defer rows.Close()
		for rows.Next() {
			var idx int
			var id, heading *string
			var content string
			if err := rows.Scan(&idx, &id, &heading, &content); err != nil {
				return nil, nil, errors.New("section fetch failed, try again shortly")
			}
			neighbor := NeighborSection{Content: content}
			if id != nil {
				neighbor.SectionID = *id
			}
			if heading != nil {
				neighbor.Heading = *heading
			}
			if len(neighbor.Content) > maxChars {
				neighbor.Content = neighbor.Content[:maxChars]
			}
			if idx < chunkIndex {
				out.ContextBefore = append(out.ContextBefore, neighbor)
			} else {
				out.ContextAfter = append(out.ContextAfter, neighbor)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, errors.New("section fetch failed, try again shortly")
		}
	}
	log.Printf("get_section: ok in %s", time.Since(start).Round(time.Millisecond))

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderSection(&out)}},
	}, &out, nil
}

func renderSection(s *SectionOutput) string {
	var sb strings.Builder
	label := s.Product
	if s.Version != "" {
		label = fmt.Sprintf("%s v%s", s.Product, s.Version)
	}
	fmt.Fprintf(&sb, "# %s (%s)\n", s.Title, label)
	if len(s.Breadcrumb) > 0 {
		fmt.Fprintf(&sb, "%s\n", strings.Join(s.Breadcrumb, " > "))
	}
	fmt.Fprintf(&sb, "Source: %s\n\n", s.CitationURL)
	for _, n := range s.ContextBefore {
		fmt.Fprintf(&sb, "<context-before heading=%q>\n%s\n</context-before>\n\n", n.Heading, n.Content)
	}
	if s.Heading != "" {
		fmt.Fprintf(&sb, "## %s\n\n", s.Heading)
	}
	sb.WriteString(s.Content)
	if s.Truncated {
		sb.WriteString("\n\n[truncated at max_chars; call get_page for the whole page]")
	}
	sb.WriteString("\n\n")
	for _, n := range s.ContextAfter {
		fmt.Fprintf(&sb, "<context-after heading=%q>\n%s\n</context-after>\n\n", n.Heading, n.Content)
	}
	return sb.String()
}

type ListDocsetsArgs struct {
	Product        string `json:"product,omitempty" jsonschema:"optional product to describe in detail; omit to list every indexed product"`
	IncludeManuals bool   `json:"include_manuals,omitempty" jsonschema:"also list the manuals in each docset; only allowed together with product"`
}

type DocsetVersion struct {
	Version         string   `json:"version" jsonschema:"empty for unversioned products"`
	Pages           int      `json:"pages"`
	Manuals         []string `json:"manuals,omitempty"`
	SourceUpdatedAt string   `json:"source_updated_at,omitempty" jsonschema:"newest page change date on the documentation site"`
	IndexedAt       string   `json:"indexed_at" jsonschema:"when this docset was last ingested"`
}

type Docset struct {
	Product       string          `json:"product"`
	Pages         int             `json:"pages"`
	LatestVersion string          `json:"latest_version" jsonschema:"empty for unversioned products"`
	Versions      []DocsetVersion `json:"versions"`
}

type ListDocsetsOutput struct {
	Docsets []Docset `json:"docsets"`
	Pages   int      `json:"pages" jsonschema:"total indexed pages across the returned docsets"`
}

// listDocsets reports what is actually indexed. product and version are
// free-form strings on every other tool, and server-side validation can only
// tell a caller that its guess was wrong; this lets it discover the valid
// values, along with how fresh each docset is.
func listDocsets(ctx context.Context, req *mcp.CallToolRequest, args ListDocsetsArgs) (*mcp.CallToolResult, *ListDocsetsOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	product := strings.TrimSpace(args.Product)
	if product != "" && !knownProducts[product] {
		return nil, nil, fmt.Errorf("unknown product %q; indexed products: %s",
			product, strings.Join(productList(), ", "))
	}
	// Manual lists are only useful scoped to one product; across the whole
	// corpus they'd be thousands of entries.
	includeManuals := args.IncludeManuals && product != ""
	log.Printf("list_docsets: product=%q include_manuals=%t", product, includeManuals)

	release, err := acquireToolSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	rows, err := pool.Query(ctx, `
		SELECT product, version, count(*) AS pages,
		       max(source_updated_at) AS source_updated_at,
		       max(updated_at) AS indexed_at,
		       CASE WHEN $2 THEN array_agg(DISTINCT manual) FILTER (WHERE manual <> '')
		            ELSE NULL END AS manuals
		FROM documents
		WHERE $1 = '' OR product = $1
		GROUP BY product, version
		ORDER BY product,
		         CASE
		             WHEN version ~ '^[0-9]+(\.[0-9]+)+$' THEN string_to_array(version, '.')::int[]
		             ELSE NULL
		         END DESC NULLS LAST,
		         version DESC
	`, product, includeManuals)
	if err != nil {
		log.Printf("list_docsets: ERROR: %v", err)
		return nil, nil, errors.New("listing docsets failed, try again shortly")
	}
	defer rows.Close()

	out := &ListDocsetsOutput{Docsets: []Docset{}}
	byProduct := map[string]*Docset{}
	for rows.Next() {
		var p, v string
		var pages int
		var sourceUpdated, indexedAt *time.Time
		var manuals []string
		if err := rows.Scan(&p, &v, &pages, &sourceUpdated, &indexedAt, &manuals); err != nil {
			return nil, nil, errors.New("listing docsets failed, try again shortly")
		}
		entry := DocsetVersion{Version: v, Pages: pages, Manuals: manuals}
		if sourceUpdated != nil {
			entry.SourceUpdatedAt = sourceUpdated.UTC().Format(time.RFC3339)
		}
		if indexedAt != nil {
			entry.IndexedAt = indexedAt.UTC().Format(time.RFC3339)
		}
		sort.Strings(entry.Manuals)
		docset, ok := byProduct[p]
		if !ok {
			out.Docsets = append(out.Docsets, Docset{Product: p, Versions: []DocsetVersion{}})
			docset = &out.Docsets[len(out.Docsets)-1]
			byProduct[p] = docset
		}
		// Rows arrive newest version first, so the first one is the latest.
		if len(docset.Versions) == 0 {
			docset.LatestVersion = v
		}
		docset.Versions = append(docset.Versions, entry)
		docset.Pages += pages
		out.Pages += pages
	}
	if err := rows.Err(); err != nil {
		return nil, nil, errors.New("listing docsets failed, try again shortly")
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderDocsets(out)}},
	}, out, nil
}

func renderDocsets(out *ListDocsetsOutput) string {
	if len(out.Docsets) == 0 {
		return "Nothing is indexed."
	}
	var sb strings.Builder
	for _, d := range out.Docsets {
		fmt.Fprintf(&sb, "## %s (%d pages)\n", d.Product, d.Pages)
		for _, v := range d.Versions {
			version := v.Version
			if version == "" {
				version = "(unversioned)"
			}
			marker := ""
			if v.Version == d.LatestVersion && len(d.Versions) > 1 {
				marker = " [latest]"
			}
			fmt.Fprintf(&sb, "- %s%s: %d pages, indexed %s", version, marker, v.Pages, v.IndexedAt)
			if v.SourceUpdatedAt != "" {
				fmt.Fprintf(&sb, ", newest source change %s", v.SourceUpdatedAt)
			}
			sb.WriteString("\n")
			if len(v.Manuals) > 0 {
				fmt.Fprintf(&sb, "  manuals: %s\n", strings.Join(v.Manuals, ", "))
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

type CompareVersionsArgs struct {
	DocumentID  string `json:"document_id,omitempty" jsonschema:"version-independent page id from a search_docs result; supply this or url"`
	URL         string `json:"url,omitempty" jsonschema:"any indexed URL of the page; the version is taken from from_version and to_version"`
	FromVersion string `json:"from_version" jsonschema:"the older version to compare, e.g. 10.2"`
	ToVersion   string `json:"to_version" jsonschema:"the newer version to compare, e.g. 10.4"`
	MaxChars    int    `json:"max_chars,omitempty" jsonschema:"truncate the rendered diff at this many characters, default 12000"`
}

type SectionChange struct {
	Heading   string `json:"heading"`
	Status    string `json:"status" jsonschema:"added, removed, or modified"`
	SectionID string `json:"section_id,omitempty" jsonschema:"section id in to_version, or in from_version for removed sections"`
	Diff      string `json:"diff,omitempty" jsonschema:"unified line diff, only for modified sections"`
}

type CompareVersionsOutput struct {
	DocumentID  string          `json:"document_id"`
	Product     string          `json:"product"`
	FromVersion string          `json:"from_version"`
	ToVersion   string          `json:"to_version"`
	FromURL     string          `json:"from_url"`
	ToURL       string          `json:"to_url"`
	Identical   bool            `json:"identical" jsonschema:"true when both versions of the page have identical text"`
	Changes     []SectionChange `json:"changes"`
	Truncated   bool            `json:"truncated"`
}

type versionedSection struct {
	sectionID string
	heading   string
	content   string
	order     int
}

// loadVersionSections returns a page's sections keyed by heading, for one
// version of one canonical page.
func loadVersionSections(ctx context.Context, canonicalID, version string) (string, map[string]versionedSection, error) {
	rows, err := pool.Query(ctx, `
		SELECT d.url, c.chunk_index, c.section_id, coalesce(c.heading, ''), c.content
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE d.canonical_id = $1 AND d.version = $2
		ORDER BY c.chunk_index
	`, canonicalID, version)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	pageURL := ""
	sections := map[string]versionedSection{}
	for rows.Next() {
		var url, heading, content string
		var idx int
		var sectionID *string
		if err := rows.Scan(&url, &idx, &sectionID, &heading, &content); err != nil {
			return "", nil, err
		}
		pageURL = url
		section := versionedSection{heading: heading, content: content, order: idx}
		if sectionID != nil {
			section.sectionID = *sectionID
		}
		// A page can repeat a heading; key on the heading plus its ordinal so
		// repeats line up between versions instead of overwriting each other.
		key := strings.ToLower(heading)
		for i := 2; ; i++ {
			if _, taken := sections[key]; !taken {
				break
			}
			key = fmt.Sprintf("%s#%d", strings.ToLower(heading), i)
		}
		sections[key] = section
	}
	return pageURL, sections, rows.Err()
}

// compareVersions reports what changed between two versions of the same page.
// Splunk research questions are frequently "what changed between X and Y", and
// the answer has to come from the source text: the diff is computed from the
// indexed content, never summarised by a model.
func compareVersions(ctx context.Context, req *mcp.CallToolRequest, args CompareVersionsArgs) (*mcp.CallToolResult, *CompareVersionsOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	canonicalID := strings.TrimSpace(args.DocumentID)
	pageURL := strings.TrimSpace(args.URL)
	from := strings.TrimSpace(args.FromVersion)
	to := strings.TrimSpace(args.ToVersion)
	if canonicalID == "" && pageURL == "" {
		return nil, nil, errors.New("supply document_id (from a search_docs result) or url")
	}
	if len(canonicalID) > maxIDLen || len(pageURL) > maxURLLen {
		return nil, nil, errors.New("document_id or url too long")
	}
	if from == "" || to == "" {
		return nil, nil, errors.New("from_version and to_version are both required; call list_docsets to see the indexed versions")
	}
	if from == to {
		return nil, nil, errors.New("from_version and to_version must differ")
	}
	maxChars := args.MaxChars
	if maxChars <= 0 || maxChars > maxSectionChars {
		maxChars = maxSectionChars
	}
	log.Printf("compare_versions: document_id=%q url=%q from=%q to=%q", canonicalID, pageURL, from, to)

	release, err := acquireToolSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	if canonicalID == "" {
		if err := pool.QueryRow(ctx, `SELECT canonical_id FROM documents WHERE url = $1`, pageURL).Scan(&canonicalID); err != nil {
			if resolved, ok := resolveURL(ctx, pageURL); ok {
				err = pool.QueryRow(ctx, `SELECT canonical_id FROM documents WHERE url = $1`, resolved).Scan(&canonicalID)
			}
			if err != nil {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
					Text: "That URL is not indexed. Use search_docs to find the page, then pass its document_id.",
				}}}, nil, nil
			}
		}
	}

	fromURL, fromSections, err := loadVersionSections(ctx, canonicalID, from)
	if err != nil {
		log.Printf("compare_versions: ERROR: %v", err)
		return nil, nil, errors.New("comparison failed, try again shortly")
	}
	toURL, toSections, err := loadVersionSections(ctx, canonicalID, to)
	if err != nil {
		log.Printf("compare_versions: ERROR: %v", err)
		return nil, nil, errors.New("comparison failed, try again shortly")
	}
	if len(fromSections) == 0 || len(toSections) == 0 {
		missing := from
		if len(toSections) == 0 {
			missing = to
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("No indexed copy of %s at version %s. Call list_docsets to see which versions are indexed.", canonicalID, missing),
		}}}, nil, nil
	}

	out := &CompareVersionsOutput{
		DocumentID:  canonicalID,
		FromVersion: from,
		ToVersion:   to,
		FromURL:     fromURL,
		ToURL:       toURL,
		Changes:     []SectionChange{},
	}
	if i := strings.Index(canonicalID, "/"); i > 0 {
		out.Product = canonicalID[:i]
	}

	keys := make([]string, 0, len(fromSections)+len(toSections))
	seen := map[string]bool{}
	for _, set := range []map[string]versionedSection{toSections, fromSections} {
		for key := range set {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	// Order by position in the newer version so the report reads like the page.
	sort.Slice(keys, func(i, j int) bool {
		return sectionOrder(toSections, fromSections, keys[i]) < sectionOrder(toSections, fromSections, keys[j])
	})

	for _, key := range keys {
		oldSection, inOld := fromSections[key]
		newSection, inNew := toSections[key]
		switch {
		case !inOld:
			out.Changes = append(out.Changes, SectionChange{
				Heading: newSection.heading, Status: "added", SectionID: newSection.sectionID,
			})
		case !inNew:
			out.Changes = append(out.Changes, SectionChange{
				Heading: oldSection.heading, Status: "removed", SectionID: oldSection.sectionID,
			})
		case oldSection.content != newSection.content:
			out.Changes = append(out.Changes, SectionChange{
				Heading:   newSection.heading,
				Status:    "modified",
				SectionID: newSection.sectionID,
				Diff:      unifiedDiff(oldSection.content, newSection.content, 2),
			})
		}
	}
	out.Identical = len(out.Changes) == 0

	text := renderComparison(out)
	if len(text) > maxChars {
		text = text[:maxChars]
		out.Truncated = true
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
}

// sectionOrder places a section by its position in the newer version, falling
// back to the older one for sections that were removed.
func sectionOrder(newer, older map[string]versionedSection, key string) int {
	if section, ok := newer[key]; ok {
		return section.order
	}
	if section, ok := older[key]; ok {
		// Removed sections sort after the surviving ones at the same index.
		return section.order*2 + 1
	}
	return 1 << 30
}

func renderComparison(out *CompareVersionsOutput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s: %s -> %s\n\n", out.DocumentID, out.FromVersion, out.ToVersion)
	fmt.Fprintf(&sb, "from: %s\nto:   %s\n\n", out.FromURL, out.ToURL)
	if out.Identical {
		sb.WriteString("The two versions of this page are textually identical.\n")
		return sb.String()
	}
	for _, change := range out.Changes {
		heading := change.Heading
		if heading == "" {
			heading = "(page intro)"
		}
		fmt.Fprintf(&sb, "## %s [%s]\n", heading, change.Status)
		if change.SectionID != "" {
			fmt.Fprintf(&sb, "section_id: %s\n", change.SectionID)
		}
		if change.Diff != "" {
			fmt.Fprintf(&sb, "\n```diff\n%s\n```\n", change.Diff)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
