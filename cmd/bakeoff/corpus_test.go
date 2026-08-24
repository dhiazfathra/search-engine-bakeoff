package main

import "testing"

// The judgement set must be true by construction: every doc the plan calls
// relevant really does contain the query terms, and no other doc does.
func TestPlantedGroundTruthHolds(t *testing.T) {
	const n = 5000
	p := buildPlan(n)
	bodies := make(map[int]string, n)
	if err := gen(n, p, func(d Doc) error { bodies[d.ID] = d.Title + " " + d.Body; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(p.queries) == 0 {
		t.Fatal("no queries")
	}
	seen := map[Shape]bool{}
	for _, q := range p.queries {
		seen[q.Shape] = true
		if q.Relevant == nil {
			continue
		}
		if len(q.Relevant) != relPerQuery {
			t.Fatalf("%v: %d relevant docs", q.Terms, len(q.Relevant))
		}
		for _, id := range q.Relevant {
			b := bodies[id]
			switch q.Shape {
			case ShapePhrase:
				if !contains(b, q.Planted[0]+" "+q.Planted[1]) {
					t.Fatalf("doc %d missing phrase %q", id, q.Planted)
				}
			default:
				for _, term := range q.Planted {
					if !contains(b, term) {
						t.Fatalf("doc %d missing %q", id, term)
					}
				}
			}
		}
	}
	for _, s := range shapes {
		if !seen[s] {
			t.Fatalf("shape %s has no queries", s)
		}
	}
}

// The typo query must differ from the planted token by exactly one character.
func TestTypoIsOneEdit(t *testing.T) {
	for _, q := range buildPlan(5000).queries {
		if q.Shape != ShapeTypo {
			continue
		}
		a, b := q.Terms[0], q.Planted[0]
		if len(a) != len(b) {
			t.Fatalf("%q vs %q", a, b)
		}
		diff := 0
		for i := range a {
			if a[i] != b[i] {
				diff++
			}
		}
		if diff != 1 {
			t.Fatalf("%q vs %q: %d edits", a, b, diff)
		}
	}
}

func contains(h, needle string) bool {
	for i := 0; i+len(needle) <= len(h); i++ {
		if h[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The generator must be deterministic: same seed, same bytes.
func TestGenIsDeterministic(t *testing.T) {
	var a, b []string
	for _, dst := range []*[]string{&a, &b} {
		p := buildPlan(5000)
		if err := gen(5000, p, func(d Doc) error { *dst = append(*dst, d.Body); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("doc %d differs between runs", i)
		}
	}
}

func TestJudge(t *testing.T) {
	rel := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	r, m := judge([]int{99, 3, 98}, rel)
	if r != 0.1 {
		t.Fatalf("recall %v", r)
	}
	if m != 0.5 {
		t.Fatalf("mrr %v", m)
	}
	if r, m := judge(nil, rel); r != 0 || m != 0 {
		t.Fatalf("empty: %v %v", r, m)
	}
}
