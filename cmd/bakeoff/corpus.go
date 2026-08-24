package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// Corpus is generated from a fixed seed so the judgement set is known by
// construction: we plant marker tokens into known document ids.

const (
	seed         = 20260824
	vocabSize    = 20000
	bodyWords    = 60
	titleWords   = 6
	queriesPerSh = 20 // per judged shape
	relPerQuery  = 10 // exactly 10 relevant docs per judged query
	distractors  = 50 // docs holding a partial signal
	commonEveryN = 20 // "zcommon" token in 1/20 of docs
)

type Shape string

const (
	ShapeCommon Shape = "common"
	ShapeRare   Shape = "rare"
	ShapeAnd    Shape = "and"
	ShapePhrase Shape = "phrase"
	ShapeTypo   Shape = "typo"
)

var shapes = []Shape{ShapeCommon, ShapeRare, ShapeAnd, ShapePhrase, ShapeTypo}

// Query is one benchmark query with its ground truth (nil Relevant => unjudged).
type Query struct {
	Shape    Shape
	Terms    []string // terms as the user typed them
	Planted  []string // tokens actually planted in the relevant docs
	Relevant []int    // doc ids known relevant by construction; nil = NOT MEASURED
}

type Doc struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// plan holds the planted tokens per doc id.
type plan struct {
	tokens  map[int][]string // appended to body as loose tokens
	phrases map[int][]string // appended as adjacent pairs
	queries []Query
}

// docsNeeded is how many distinct documents the judgement set consumes.
const docsNeeded = queriesPerSh * (relPerQuery + (relPerQuery + 2*distractors) + (relPerQuery + distractors))

func pickDocs(r *rand.Rand, n, k int, used map[int]bool) []int {
	if len(used)+k > n {
		panic(fmt.Sprintf("corpus too small: need at least %d docs", docsNeeded))
	}
	out := make([]int, 0, k)
	for len(out) < k {
		id := r.Intn(n)
		if used[id] {
			continue
		}
		used[id] = true
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// typoPos is the index of the discriminating block in a rare token
// ("zrare" + 4 identical letters + "term"); corrupting one of those letters
// puts the query at edit distance 1 from its own planted token and at least 3
// from every other, so a fuzzy match cannot be accidentally right.
const typoPos = 5

func typo(s string) string {
	b := []byte(s)
	b[typoPos] = 'z'
	return string(b)
}

func rareToken(i int) string {
	return "zrare" + strings.Repeat(string(rune('a'+i)), 4) + "term"
}

func buildPlan(n int) *plan {
	r := rand.New(rand.NewSource(seed))
	p := &plan{tokens: map[int][]string{}, phrases: map[int][]string{}}
	used := map[int]bool{} // a doc carries at most one planted signal

	add := func(id int, tok ...string) { p.tokens[id] = append(p.tokens[id], tok...) }

	for i := 0; i < queriesPerSh; i++ {
		// rare single term
		tok := rareToken(i)
		rel := pickDocs(r, n, relPerQuery, used)
		for _, id := range rel {
			add(id, tok)
		}
		p.queries = append(p.queries, Query{ShapeRare, []string{tok}, []string{tok}, rel})
		// the same planted docs answer the typo query — one edit away
		p.queries = append(p.queries, Query{ShapeTypo, []string{typo(tok)}, []string{tok}, rel})
	}

	for i := 0; i < queriesPerSh; i++ {
		a := fmt.Sprintf("zandl%03dleft", i)
		b := fmt.Sprintf("zandr%03dright", i)
		rel := pickDocs(r, n, relPerQuery, used)
		for _, id := range rel {
			add(id, a, b)
		}
		for _, id := range pickDocs(r, n, distractors, used) {
			add(id, a)
		}
		for _, id := range pickDocs(r, n, distractors, used) {
			add(id, b)
		}
		p.queries = append(p.queries, Query{ShapeAnd, []string{a, b}, []string{a, b}, rel})
	}

	for i := 0; i < queriesPerSh; i++ {
		a := fmt.Sprintf("zphra%03dhead", i)
		b := fmt.Sprintf("zphrb%03dtail", i)
		rel := pickDocs(r, n, relPerQuery, used)
		for _, id := range rel {
			p.phrases[id] = append(p.phrases[id], a+" "+b)
		}
		// distractors contain both tokens but never adjacent (loose tokens are
		// spread across the body by the generator)
		for _, id := range pickDocs(r, n, distractors, used) {
			add(id, a, b)
		}
		p.queries = append(p.queries, Query{ShapePhrase, []string{a, b}, []string{a, b}, rel})
	}

	// common term: thousands of relevant docs, so relevance is NOT MEASURED.
	p.queries = append(p.queries, Query{ShapeCommon, []string{"zcommon"}, []string{"zcommon"}, nil})
	return p
}

// gen streams the corpus deterministically.
func gen(n int, p *plan, emit func(Doc) error) error {
	r := rand.New(rand.NewSource(seed + 1))
	vocab := make([]string, vocabSize)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("w%05d", i)
	}
	// zipf-ish: square the uniform to bias towards low indices
	word := func() string {
		u := r.Float64()
		return vocab[int(u*u*float64(vocabSize))]
	}
	var sb strings.Builder
	for id := 0; id < n; id++ {
		sb.Reset()
		for i := 0; i < titleWords; i++ {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(word())
		}
		title := sb.String()

		loose := p.tokens[id]
		sb.Reset()
		li := 0
		for i := 0; i < bodyWords; i++ {
			if i > 0 {
				sb.WriteByte(' ')
			}
			// spread planted loose tokens far apart so they are never adjacent
			if li < len(loose) && i > 0 && i%17 == 0 {
				sb.WriteString(loose[li])
				li++
				continue
			}
			sb.WriteString(word())
		}
		for ; li < len(loose); li++ {
			sb.WriteByte(' ')
			sb.WriteString(loose[li])
		}
		for _, ph := range p.phrases[id] {
			sb.WriteByte(' ')
			sb.WriteString(ph)
		}
		if id%commonEveryN == 0 {
			sb.WriteString(" zcommon")
		}
		if err := emit(Doc{ID: id, Title: title, Body: sb.String()}); err != nil {
			return err
		}
	}
	return nil
}
