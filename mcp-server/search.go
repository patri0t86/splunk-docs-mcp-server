package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pgvector/pgvector-go"
)

// Retrieval modes. "auto" inspects the query for technical identifiers and
// weights the literal leg accordingly; "exact" and "conceptual" let a caller
// override that when it already knows what kind of question it is asking.
const (
	modeAuto       = "auto"
	modeExact      = "exact"
	modeConceptual = "conceptual"
)

// candidatePool is how many chunks each retrieval leg contributes to the
// fusion. Larger than the result count on purpose: fusion, version collapsing
// and per-page diversity all discard rows, so the pools have to be deep
// enough that a filtered query still has something left to rank.
const candidatePool = 60

// maxSectionsPerPage caps how many sections of the same page may appear in
// one result set, so a single long reference page cannot occupy every slot.
const maxSectionsPerPage = 2

type SearchArgs struct {
	Query string `json:"query" jsonschema:"the search query"`
	// Product/Version are the original single-value filters; Products and
	// Versions supersede them. Both are accepted so existing callers keep
	// working.
	Product    string   `json:"product,omitempty" jsonschema:"optional single product filter; use products to pass several"`
	Products   []string `json:"products,omitempty" jsonschema:"optional list of products to restrict the search to, e.g. splunk-enterprise, splunk-cloud-platform, splunk-enterprise-security-8, splunk-soar, splunk-it-service-intelligence, splunk-lantern, splunk-dev, splunk-ui. Call list_docsets for the indexed values."`
	Version    string   `json:"version,omitempty" jsonschema:"optional single version filter; use versions to pass several"`
	Versions   []string `json:"versions,omitempty" jsonschema:"optional list of versions to restrict the search to, e.g. 10.2 (Splunk Enterprise), 10.5.2605 (Splunk Cloud release train), 8.5 (Enterprise Security). Call list_docsets for the indexed values."`
	Manuals    []string `json:"manuals,omitempty" jsonschema:"optional list of manuals to restrict the search to, e.g. distributed-deployment-manual. Call list_docsets with include_manuals for the indexed values."`
	Mode       string   `json:"mode,omitempty" jsonschema:"retrieval mode: auto (default, detects technical identifiers), exact (favour literal identifier matches such as props.conf or tstats), or conceptual (favour meaning over wording)"`
	LatestOnly *bool    `json:"latest_only,omitempty" jsonschema:"collapse copies of the same page across product versions and keep only the newest, default true; set false to see every version"`
	MaxResults int      `json:"max_results,omitempty" jsonschema:"maximum number of results to return, default 5"`
}

// SearchResult is one retrieved section. It carries enough identity
// (section_id, document_id, url + anchor, content_hash) for the caller to
// cite it and to fetch it back with get_section or get_page.
type SearchResult struct {
	Rank            int      `json:"rank" jsonschema:"1-based position in this result set"`
	SectionID       string   `json:"section_id" jsonschema:"stable identifier of this section, pass to get_section"`
	DocumentID      string   `json:"document_id" jsonschema:"version-independent identifier of the page, pass to compare_versions"`
	Title           string   `json:"title"`
	Product         string   `json:"product"`
	Version         string   `json:"version" jsonschema:"empty for unversioned products such as splunk-lantern"`
	Manual          string   `json:"manual"`
	Breadcrumb      []string `json:"breadcrumb"`
	Heading         string   `json:"heading" jsonschema:"heading of the matched section, empty for a page intro"`
	URL             string   `json:"url"`
	Anchor          string   `json:"anchor" jsonschema:"heading anchor within the page"`
	CitationURL     string   `json:"citation_url" jsonschema:"url with the section anchor applied"`
	Snippet         string   `json:"snippet" jsonschema:"matched text with query terms marked by ** **"`
	MatchTypes      []string `json:"match_types" jsonschema:"which retrieval legs matched: keyword, partial, identifier, semantic, title, heading"`
	ContentHash     string   `json:"content_hash" jsonschema:"hash of the source page, changes when the page changes"`
	SourceUpdatedAt string   `json:"source_updated_at,omitempty" jsonschema:"when the documentation site last changed the page"`
}

type SearchOutput struct {
	Query   string         `json:"query"`
	Mode    string         `json:"mode" jsonschema:"the retrieval mode actually used"`
	Results []SearchResult `json:"results"`
	Count   int            `json:"count"`
}

// orQuery rejoins the query terms with OR so the broad lexical leg can rank
// partial matches. websearch_to_tsquery understands the OR keyword, and
// ts_rank_cd still rewards chunks matching more of the terms, so this widens
// recall without flattening the ranking.
func orQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) < 2 {
		return q
	}
	return strings.Join(fields, " OR ")
}

// splCommands are the SPL commands common enough in questions that treating
// them as literal identifiers materially improves retrieval. The English
// stemmer mangles several of them (stats/stat, fields/field), and none of
// them mean what their stem means.
var splCommands = map[string]bool{
	"tstats": true, "stats": true, "eventstats": true, "streamstats": true,
	"timechart": true, "chart": true, "mstats": true, "mcollect": true,
	"eval": true, "where": true, "rex": true, "regex": true, "erex": true,
	"lookup": true, "inputlookup": true, "outputlookup": true, "spath": true,
	"transaction": true, "dedup": true, "mvexpand": true, "makemv": true,
	"makeresults": true, "fillnull": true, "bin": true, "bucket": true,
	"appendcols": true, "appendpipe": true, "collect": true, "datamodel": true,
	"tscollect": true, "metadata": true, "metasearch": true, "savedsearch": true,
	"iplocation": true, "geostats": true, "cluster": true, "kmeans": true,
	"anomalydetection": true, "autoregress": true, "trendline": true,
	"xyseries": true, "untable": true, "transpose": true, "foreach": true,
	"multisearch": true, "sendalert": true, "summaryindex": true, "rest": true,
}

// identifierTokenRe matches a token that carries punctuation an English
// tsvector would throw away: dotted names (props.conf, index.conf),
// underscored fields (_time, host_name), namespaced values (source::/var/log),
// REST and file paths, dashed error codes, and the SHOUTING-class conf
// attributes (TRANSFORMS-null, EVAL-severity) whose class suffix is free-form.
var identifierTokenRe = regexp.MustCompile(`^(?:[A-Za-z0-9]+(?:[._][A-Za-z0-9]+)+|_[A-Za-z0-9]+|[A-Za-z]+::.+|/[A-Za-z0-9._/-]+|[A-Z][A-Z0-9]{2,}(?:[-_][A-Za-z0-9]+)+)$`)

// identifierTokens extracts the literal, punctuation-bearing identifiers from
// a query. Splunk questions are full of them (props.conf, _time, tstats,
// /services/search/jobs, SSL-0001) and the English full-text configuration
// stems or splits every one, so they need a literal retrieval leg of their
// own to be found reliably.
func identifierTokens(q string) []string {
	seen := map[string]bool{}
	var tokens []string
	for _, field := range strings.Fields(q) {
		token := strings.Trim(field, `"'.,;:()[]{}?!`)
		token = strings.TrimPrefix(token, "|")
		if token == "" || seen[strings.ToLower(token)] {
			continue
		}
		if identifierTokenRe.MatchString(token) || splCommands[strings.ToLower(token)] {
			seen[strings.ToLower(token)] = true
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// legWeights returns the reciprocal-rank-fusion weight of each retrieval leg
// for a mode. Raw vector distances are never exposed or compared across legs:
// fusion happens on ranks, so no leg's score scale can dominate another's.
func legWeights(mode string, hasIdentifiers bool) (strict, broad, exact, semantic float64) {
	switch mode {
	case modeExact:
		return 1.0, 0.5, 2.0, 0.3
	case modeConceptual:
		return 1.0, 0.5, 0.0, 2.0
	default:
		if hasIdentifiers {
			return 1.0, 0.4, 1.2, 0.9
		}
		return 1.0, 0.4, 0.3, 1.0
	}
}

// mergeFilter combines the legacy single-value filter with its list form and
// drops empties, so callers can use either spelling. The result is never nil:
// pgx encodes a nil slice as SQL NULL, and the query's filters compare against
// an empty array.
func mergeFilter(single string, list []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range append([]string{single}, list...) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// searchQuerySQL fuses four retrieval legs with reciprocal rank fusion, then
// collapses version copies and enforces per-page diversity.
//
// The legs are deliberately independent rather than sequential: the previous
// design only widened the keyword query when the strict query matched nothing
// at all, so a single weak strict hit could suppress every useful partial
// match. Running strict and broad as separate legs lets both contribute.
//
// Collapsing keys on canonical_id (product + manual + version-free page path)
// and the section anchor, never on the title: the corpus contains many
// distinct pages titled "Troubleshooting" or "Prerequisites" that would
// otherwise suppress one another, while the 10.2 and 10.4 copies of one page
// have to collapse into a single result.
var searchQuerySQL = `
WITH strict_leg AS (
	SELECT id, row_number() OVER (ORDER BY r DESC, id) AS rank
	FROM (
		SELECT c.id, ts_rank_cd(c.tsv, websearch_to_tsquery('english', $1), 32) AS r
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.tsv @@ websearch_to_tsquery('english', $1)
		  AND (cardinality($5::text[]) = 0 OR d.product = ANY($5))
		  AND (cardinality($6::text[]) = 0 OR d.version = ANY($6))
		  AND (cardinality($7::text[]) = 0 OR d.manual = ANY($7))
		ORDER BY r DESC
		LIMIT ` + fmt.Sprint(candidatePool) + `
	) s
),
broad_leg AS (
	SELECT id, row_number() OVER (ORDER BY r DESC, id) AS rank
	FROM (
		SELECT c.id, ts_rank_cd(c.tsv, websearch_to_tsquery('english', $2), 32) AS r
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE $2 <> '' AND c.tsv @@ websearch_to_tsquery('english', $2)
		  AND (cardinality($5::text[]) = 0 OR d.product = ANY($5))
		  AND (cardinality($6::text[]) = 0 OR d.version = ANY($6))
		  AND (cardinality($7::text[]) = 0 OR d.manual = ANY($7))
		ORDER BY r DESC
		LIMIT ` + fmt.Sprint(candidatePool) + `
	) s
),
exact_leg AS (
	SELECT id, row_number() OVER (ORDER BY r DESC, id) AS rank
	FROM (
		SELECT c.id, ts_rank_cd(c.tsv_simple, websearch_to_tsquery('simple', $3), 32) AS r
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE $3 <> '' AND $10::float8 > 0 AND c.tsv_simple @@ websearch_to_tsquery('simple', $3)
		  AND (cardinality($5::text[]) = 0 OR d.product = ANY($5))
		  AND (cardinality($6::text[]) = 0 OR d.version = ANY($6))
		  AND (cardinality($7::text[]) = 0 OR d.manual = ANY($7))
		ORDER BY r DESC
		LIMIT ` + fmt.Sprint(candidatePool) + `
	) s
),
semantic_leg AS (
	SELECT id, row_number() OVER (ORDER BY dist, id) AS rank
	FROM (
		SELECT c.id, c.embedding <=> $4 AS dist
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE $11::float8 > 0
		  AND (cardinality($5::text[]) = 0 OR d.product = ANY($5))
		  AND (cardinality($6::text[]) = 0 OR d.version = ANY($6))
		  AND (cardinality($7::text[]) = 0 OR d.manual = ANY($7))
		ORDER BY dist
		LIMIT ` + fmt.Sprint(candidatePool) + `
	) s
),
legs AS (
	SELECT id, 'keyword'::text    AS leg, $8::float8  / (60 + rank) AS w FROM strict_leg
	UNION ALL
	SELECT id, 'partial'::text    AS leg, $9::float8  / (60 + rank) AS w FROM broad_leg
	UNION ALL
	SELECT id, 'identifier'::text AS leg, $10::float8 / (60 + rank) AS w FROM exact_leg
	UNION ALL
	SELECT id, 'semantic'::text   AS leg, $11::float8 / (60 + rank) AS w FROM semantic_leg
),
fused AS (
	SELECT id, sum(w) AS score, array_agg(DISTINCT leg) FILTER (WHERE w > 0) AS legs
	FROM legs
	GROUP BY id
),
scored AS (
	SELECT d.url, d.title, d.product, d.version, d.manual, d.breadcrumb,
	       d.canonical_id, d.content_hash, d.source_updated_at,
	       c.content, c.heading, c.anchor, c.section_id,
	       coalesce(nullif(split_part(coalesce(c.section_id, ''), '#', 2), ''),
	                lower(coalesce(c.heading, ''))) AS section_key,
	       coalesce(f.legs, ARRAY[]::text[])
	       || CASE WHEN to_tsvector('english', d.title) @@ websearch_to_tsquery('english', $1)
	               THEN ARRAY['title'] ELSE ARRAY[]::text[] END
	       || CASE WHEN to_tsvector('english', coalesce(c.heading, '')) @@ websearch_to_tsquery('english', $1)
	               THEN ARRAY['heading'] ELSE ARRAY[]::text[] END AS match_types,
	       -- Exact title and heading matches are promoted ahead of body-only
	       -- matches. The bonuses are on the scale of one fused rank step, so
	       -- they reorder near-ties without overriding the fusion.
	       f.score
	       + CASE WHEN to_tsvector('english', d.title) @@ websearch_to_tsquery('english', $1) THEN 0.010 ELSE 0 END
	       + CASE WHEN to_tsvector('english', coalesce(c.heading, '')) @@ websearch_to_tsquery('english', $1) THEN 0.005 ELSE 0 END
	       AS score
	FROM fused f
	JOIN chunks c ON c.id = f.id
	JOIN documents d ON d.id = c.document_id
),
collapsed AS (
	SELECT *,
	       row_number() OVER (
	           PARTITION BY canonical_id, section_key,
	                        CASE WHEN $12::boolean THEN '' ELSE version END
	           ORDER BY
	               CASE
	                   WHEN version ~ '^[0-9]+(\.[0-9]+)+$' THEN string_to_array(version, '.')::int[]
	                   ELSE NULL
	               END DESC NULLS LAST,
	               score DESC
	       ) AS version_rn
	FROM scored
),
diverse AS (
	SELECT *, row_number() OVER (PARTITION BY canonical_id ORDER BY score DESC) AS page_rn
	FROM collapsed
	WHERE version_rn = 1
)
SELECT section_id, canonical_id, title, product, version, manual, breadcrumb,
       heading, url, anchor, content_hash, source_updated_at, match_types,
       CASE
           WHEN match_types && ARRAY['keyword', 'partial', 'title', 'heading'] THEN
               ts_headline('english', content, websearch_to_tsquery('english', coalesce(nullif($2, ''), $1)),
                   'StartSel=**, StopSel=**, MaxFragments=3, MaxWords=50, MinWords=15, FragmentDelimiter=" … "')
           WHEN match_types && ARRAY['identifier'] THEN
               ts_headline('simple', content, websearch_to_tsquery('simple', $3),
                   'StartSel=**, StopSel=**, MaxFragments=3, MaxWords=50, MinWords=15, FragmentDelimiter=" … "')
           ELSE left(content, 500)
       END AS snippet
FROM diverse
WHERE page_rn <= $13::int
ORDER BY score DESC
LIMIT $14::int
`

// searchDocs retrieves documentation sections with four independent legs --
// strict keyword, broad keyword, literal identifier, and semantic -- fused by
// reciprocal rank. It returns structured results alongside the rendered text
// so a client can cite a specific section rather than a whole page.
func searchDocs(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, *SearchOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return nil, nil, errors.New("query must not be empty")
	}
	if len(args.Query) > maxQueryLen {
		return nil, nil, fmt.Errorf("query too long (max %d bytes)", maxQueryLen)
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 5
	}
	if args.MaxResults > maxResultsCap {
		args.MaxResults = maxResultsCap
	}

	products := mergeFilter(args.Product, args.Products)
	for _, p := range products {
		if !knownProducts[p] {
			return nil, nil, fmt.Errorf("unknown product %q; indexed products: %s",
				p, strings.Join(productList(), ", "))
		}
	}
	versions := mergeFilter(args.Version, args.Versions)
	manuals := mergeFilter("", args.Manuals)

	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	switch mode {
	case "", modeAuto:
		mode = modeAuto
	case modeExact, modeConceptual:
	default:
		return nil, nil, fmt.Errorf("unknown mode %q; use auto, exact, or conceptual", args.Mode)
	}

	latestOnly := args.LatestOnly == nil || *args.LatestOnly

	identifiers := identifierTokens(args.Query)
	exactQuery := args.Query
	if len(identifiers) > 0 {
		exactQuery = strings.Join(identifiers, " OR ")
	}
	wStrict, wBroad, wExact, wSemantic := legWeights(mode, len(identifiers) > 0)

	log.Printf("search_docs: query=%q mode=%s products=%v versions=%v manuals=%v latest_only=%t identifiers=%v max_results=%d",
		args.Query, mode, products, versions, manuals, latestOnly, identifiers, args.MaxResults)

	release, err := acquireToolSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	start := time.Now()
	// nomic-embed-text queries must carry the search_query prefix to match the
	// search_document-prefixed vectors written by the indexer.
	vec, err := embed(ctx, "search_query: "+args.Query)
	if err != nil {
		log.Printf("search_docs: ERROR embedding query after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return nil, nil, errors.New("search backend unavailable, try again shortly")
	}
	queryVec := pgvector.NewVector(vec)
	log.Printf("search_docs: embedded query in %s (%d dims)", time.Since(start).Round(time.Millisecond), len(vec))

	dbStart := time.Now()
	// Product/version/manual filters run inside each leg, not after: with
	// several products in one index, a post-filter would let the dominant
	// products crowd a filtered query's candidates out of the fixed pools.
	rows, err := pool.Query(ctx, searchQuerySQL,
		args.Query, orQuery(args.Query), exactQuery, queryVec,
		products, versions, manuals,
		wStrict, wBroad, wExact, wSemantic,
		latestOnly, maxSectionsPerPage, args.MaxResults)
	if err != nil {
		log.Printf("search_docs: ERROR db query after %s: %v", time.Since(dbStart).Round(time.Millisecond), err)
		return nil, nil, errors.New("search failed, try again shortly")
	}
	defer rows.Close()

	out := &SearchOutput{Query: args.Query, Mode: mode, Results: []SearchResult{}}
	for rows.Next() {
		var r SearchResult
		var sectionID, anchor, breadcrumb *string
		var sourceUpdated *time.Time
		if err := rows.Scan(&sectionID, &r.DocumentID, &r.Title, &r.Product, &r.Version,
			&r.Manual, &breadcrumb, &r.Heading, &r.URL, &anchor, &r.ContentHash,
			&sourceUpdated, &r.MatchTypes, &r.Snippet); err != nil {
			log.Printf("search_docs: ERROR scanning row: %v", err)
			return nil, nil, errors.New("search failed, try again shortly")
		}
		if sectionID != nil {
			r.SectionID = *sectionID
		}
		if anchor != nil {
			r.Anchor = *anchor
		}
		if breadcrumb != nil && *breadcrumb != "" {
			r.Breadcrumb = strings.Split(*breadcrumb, " > ")
		}
		if sourceUpdated != nil {
			r.SourceUpdatedAt = sourceUpdated.UTC().Format(time.RFC3339)
		}
		r.CitationURL = r.URL
		if r.Anchor != "" && r.Anchor != "overview" {
			r.CitationURL = r.URL + "#" + r.Anchor
		}
		sort.Strings(r.MatchTypes)
		r.Rank = len(out.Results) + 1
		out.Results = append(out.Results, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("search_docs: ERROR reading rows after %s: %v", time.Since(dbStart).Round(time.Millisecond), err)
		return nil, nil, errors.New("search failed, try again shortly")
	}
	out.Count = len(out.Results)
	log.Printf("search_docs: db returned %d results in %s (total %s)",
		out.Count, time.Since(dbStart).Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderSearchResults(out)}},
	}, out, nil
}

// renderSearchResults is the text rendering of the structured results, kept
// for clients that don't read structuredContent.
func renderSearchResults(out *SearchOutput) string {
	if len(out.Results) == 0 {
		return "No matching results. Try fewer terms, mode \"conceptual\", or drop the product/version filters (list_docsets shows what is indexed)."
	}
	var sb strings.Builder
	for _, r := range out.Results {
		label := r.Product
		if r.Version != "" {
			label = fmt.Sprintf("%s v%s", r.Product, r.Version)
		}
		fmt.Fprintf(&sb, "## %d. %s (%s)\n", r.Rank, r.Title, label)
		if len(r.Breadcrumb) > 0 {
			fmt.Fprintf(&sb, "%s\n", strings.Join(r.Breadcrumb, " > "))
		}
		if r.Heading != "" {
			fmt.Fprintf(&sb, "Section: %s\n", r.Heading)
		}
		fmt.Fprintf(&sb, "%s\n", r.CitationURL)
		fmt.Fprintf(&sb, "section_id: %s | matched: %s\n\n%s\n\n---\n\n",
			r.SectionID, strings.Join(r.MatchTypes, ", "), r.Snippet)
	}
	return sb.String()
}
