package semantic

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// splitTokens lowercases text and splits it into word tokens on every
// non-alphanumeric rune and on camelCase boundaries.
//
// Splitting camelCase is what lets a requirement mentioning "ResumeOnWindows"
// correlate with resume_windows.go or TestResumeOnWindows. Keyword overlap is
// the only bridge this extractor has between prose and repository facts, and
// plan §31 wants every claim anchored to a Level 1 artifact, so the bridge has
// to survive naming conventions.
func splitTokens(s string) []string {
	var (
		out  []string
		cur  []rune
		prev rune
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsUpper(r):
			// A capital after a lower-case letter or digit starts a new word:
			// "ResumeOnWindows" -> resume, on, windows.
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				flush()
			}
			cur = append(cur, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur = append(cur, r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

// tokenSet returns every token in the text, unfiltered. Cue matching uses this
// rather than keywords: a cue like "add" is exactly the sort of short, common
// word keyword filtering removes.
func tokenSet(s string) map[string]bool {
	toks := splitTokens(s)
	m := make(map[string]bool, len(toks))
	for _, t := range toks {
		m[t] = true
	}
	return m
}

// stopWords are tokens too common, or too structural, to carry meaning in a
// correlation.
//
// Path scaffolding ("internal", "pkg", "cmd", "src") and "test" are in here on
// purpose: without them every requirement would correlate with every file under
// internal/, manufacturing evidence links that do not exist. Fabricated evidence
// is worse than no evidence (plan §32).
var stopWordList = []string{
	"all", "also", "and", "any", "are", "back", "been", "both", "but", "can",
	"cmd", "does", "each", "for", "from", "get", "had", "has", "have", "her",
	"here", "him", "his", "how", "internal", "into", "its", "just", "like",
	"make", "more", "most", "much", "new", "non", "not", "now", "old", "one",
	"only", "our", "out", "over", "own", "pkg", "put", "same", "set", "she",
	"some", "src", "still", "such", "test", "tests", "than", "that", "the",
	"their", "them", "then", "there", "these", "they", "this", "those", "too",
	"two", "use", "used", "uses", "using", "very", "was", "way", "were", "what",
	"when", "where", "which", "while", "who", "why", "will", "with", "would",
	"you", "your",
}

// noiseWords is stopWordList plus every cue word. Cues are structural markers of
// how a sentence was phrased, not of what it is about: correlating a
// requirement to a file because both contain the word "add" would be a false
// evidence link.
//
// It is built by iterating fixed slices, never a map, so the result cannot vary
// between runs.
var noiseWords = func() map[string]bool {
	m := make(map[string]bool, len(stopWordList)+32)
	for _, w := range stopWordList {
		m[w] = true
	}
	for _, list := range [][]cue{requirementCues, decisionCues, rejectionCues, assumptionCues} {
		for _, c := range list {
			for _, t := range splitTokens(string(c)) {
				m[t] = true
			}
		}
	}
	return m
}()

// keywords returns the sorted, unique, meaning-bearing tokens of a text. Tokens
// shorter than three characters are dropped along with noise words; what
// remains is what correlation is allowed to match on.
func keywords(s string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range splitTokens(s) {
		if len(t) < 3 || noiseWords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// keywordSet is keywords as a membership set, for the right-hand side of an
// overlap test.
func keywordSet(parts ...string) map[string]bool {
	m := make(map[string]bool)
	for _, p := range parts {
		for _, k := range keywords(p) {
			m[k] = true
		}
	}
	return m
}

// overlap counts how many of the sorted keywords appear in the candidate set.
// Iteration is over the sorted slice, never the map, so the count and any
// tie-break derived from it are stable.
func overlap(kw []string, candidate map[string]bool) int {
	n := 0
	for _, k := range kw {
		if candidate[k] {
			n++
		}
	}
	return n
}

// match reports whether the cue fires on a text.
//
// A multi-word cue ("instead of", "needs to") is matched as a substring because
// its words carry no signal apart. A single-word cue is matched on a token
// boundary instead of as a substring, so "add" does not fire on "address" and
// "broke" does not fire on "brokerage". Inflections are deliberately not
// matched: the vocabulary in semantic.go is the whole rule, and a reader must be
// able to predict the output from it.
func (c cue) match(lower string, toks map[string]bool) bool {
	s := string(c)
	if strings.ContainsRune(s, ' ') {
		return strings.Contains(lower, s)
	}
	return toks[s]
}

// indexFold returns the byte offset in s of the first case-insensitive match of
// sub, or -1. lastIndexFold returns the offset of the last one.
//
// These exist instead of the obvious strings.Index(strings.ToLower(s), sub)
// because that idiom is wrong whenever the result is used to slice s.
// strings.ToLower can change a string's byte length — U+023A lower-cases to a
// three-byte rune, U+023E likewise — so an offset found in the lower-cased copy
// is not a valid offset into the original, and slicing with it panics. The
// offsets returned here always index s itself.
func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func lastIndexFold(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// matchCue returns the first cue in the vocabulary that fires, and whether any
// did. First-match order is the vocabulary's own order, which is fixed.
func matchCue(text string, cues []cue) (cue, bool) {
	lower := strings.ToLower(text)
	toks := tokenSet(text)
	for _, c := range cues {
		if c.match(lower, toks) {
			return c, true
		}
	}
	return "", false
}

// sentenceSplit is deliberately crude: it breaks on terminal punctuation only.
// Abbreviations such as "e.g." will split, which costs a little precision and
// buys a rule that behaves identically on every input forever.
var sentenceSplit = regexp.MustCompile(`[.!?]+\s+|[.!?]+$`)

// sentences splits a line into trimmed, non-empty sentences.
func sentences(line string) []string {
	var out []string
	for _, s := range sentenceSplit.Split(line, -1) {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// markerNumbered matches "R1 - x", "REQ 2: x", "3. x" and "4) x".
var markerNumbered = regexp.MustCompile(`^(?i:(?:r|req)\s*)?\d+\s*[-–—.):]\s*(.+)$`)

// markerBullet matches "- x", "* x" and "• x".
var markerBullet = regexp.MustCompile(`^[-*•]\s+(.+)$`)

// stripMarker removes a list marker from a line and reports which form it was.
// The form is carried onto the evidence record so a reader can see why a line
// became a requirement, rather than having to trust that it did.
func stripMarker(line string) (text, form string, ok bool) {
	if m := markerNumbered.FindStringSubmatch(line); m != nil {
		return m[1], "numbered", true
	}
	if m := markerBullet.FindStringSubmatch(line); m != nil {
		return m[1], "bullet", true
	}
	return "", "", false
}

// tidy trims surrounding space and trailing list punctuation so requirement text
// renders cleanly as a bullet in the handoff. It never rewords the source.
func tidy(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), " \t.,;:")
}

// symbolFromTest derives the symbol a test exercises from its name:
// "TestResumeOnWindows/subcase" -> "ResumeOnWindows", "test_resume" -> "resume".
// It returns "" when nothing is derivable, because an empty Target is honest and
// a guessed one would send the Graph verification layer at a symbol that does
// not exist.
func symbolFromTest(name string) string {
	n := strings.TrimSpace(name)
	// A reporter may qualify the name with a package path
	// ("internal/store.TestAppend"). Anchor on the last ".test" rather than on
	// the last dot: a Go subtest name can itself contain dots, and anchoring on
	// the last dot in "internal/store.TestAppend/case.1" fails to recognise the
	// qualifier, leaving the package path in place so that the "/" trim below
	// returns "internal" — a directory handed to Graph as if it were a symbol.
	// Inventing a target is exactly what this function exists not to do.
	if i := lastIndexFold(n, ".test"); i >= 0 {
		n = n[i+1:]
	}
	if i := strings.Index(n, "/"); i >= 0 { // Go subtests append "/case"
		n = n[:i]
	}
	if len(n) >= 4 && strings.EqualFold(n[:4], "test") {
		n = strings.TrimLeft(n[4:], "_-")
	}
	if n == "" {
		return ""
	}
	return n
}
