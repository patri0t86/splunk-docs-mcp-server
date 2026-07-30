package main

import (
	"fmt"
	"strings"
)

// maxDiffLines bounds the input to the diff so a pathological pair of long
// sections can't cost quadratic time and memory.
const maxDiffLines = 400

// unifiedDiff renders a line-level unified diff of two section bodies with
// the given number of context lines. It is a plain LCS diff rather than a
// summary: version comparisons have to report the source text that changed,
// not a paraphrase of it.
func unifiedDiff(oldText, newText string, context int) string {
	oldLines := splitDiffLines(oldText)
	newLines := splitDiffLines(newText)

	// lcs[i][j] is the length of the longest common subsequence of
	// oldLines[i:] and newLines[j:].
	lcs := make([][]int, len(oldLines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	type diffLine struct {
		op   byte // ' ', '-', '+'
		text string
	}
	var lines []diffLine
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		switch {
		case oldLines[i] == newLines[j]:
			lines = append(lines, diffLine{' ', oldLines[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			lines = append(lines, diffLine{'-', oldLines[i]})
			i++
		default:
			lines = append(lines, diffLine{'+', newLines[j]})
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		lines = append(lines, diffLine{'-', oldLines[i]})
	}
	for ; j < len(newLines); j++ {
		lines = append(lines, diffLine{'+', newLines[j]})
	}

	// No changed line survived: either the texts are equal, or the change lay
	// beyond the truncation window. Emitting only "@@ n unchanged lines @@"
	// would read as a change, so report nothing instead.
	changed := false
	for _, line := range lines {
		if line.op != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}

	// Keep changed lines plus the requested context; collapse the rest, which
	// on near-identical version copies is nearly the whole section.
	keep := make([]bool, len(lines))
	for idx, line := range lines {
		if line.op == ' ' {
			continue
		}
		for k := max(0, idx-context); k <= min(len(lines)-1, idx+context); k++ {
			keep[k] = true
		}
	}

	var sb strings.Builder
	skipped := 0
	flushSkipped := func() {
		if skipped > 0 {
			fmt.Fprintf(&sb, "@@ %d unchanged line(s) @@\n", skipped)
			skipped = 0
		}
	}
	for idx, line := range lines {
		if !keep[idx] {
			skipped++
			continue
		}
		flushSkipped()
		sb.WriteByte(line.op)
		sb.WriteString(line.text)
		sb.WriteByte('\n')
	}
	flushSkipped()
	return strings.TrimRight(sb.String(), "\n")
}

// splitDiffLines splits a section body into diff units, truncating very long
// sections so the quadratic LCS stays bounded.
func splitDiffLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > maxDiffLines {
		lines = append(lines[:maxDiffLines:maxDiffLines], "[section truncated for diff]")
	}
	return lines
}
