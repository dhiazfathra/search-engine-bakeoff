# 0001 — Where Postgres full-text search stops being enough

## Status

Accepted

## Date

2026-08-24

## Context

The author previously migrated a service from Postgres full-text search to
Elasticsearch and got a large latency win. That migration became the default
advice given to every team afterward — "just use Elasticsearch" — without ever
establishing at what corpus size or query shape Postgres actually stopped
being viable. That advice costs a team an ops budget (a cluster to run, JVM
heaps to tune, a second datastore to keep consistent with Postgres) it may not
need.

To answer this honestly we built `search-engine-bakeoff`: a seeded corpus with
a judgement set constructed by planting marker tokens into known document ids
(so recall@10/MRR@10 are computed against ground truth, not guessed), and a Go
harness that indexes Postgres 17 (tuned: stored generated `tsvector` + GIN,
`work_mem`/`shared_buffers`/`maintenance_work_mem` set, `ANALYZE` run),
Elasticsearch 8.19 (2 GB heap, single shard, force-merged), and Meilisearch
1.24 one at a time and measures p50/p95/p99, throughput, index size, build
time, and steady-state RSS per query shape (rare term, two-term AND, phrase,
prefix/typo, and a high-frequency common term).

Full numbers: `results/n1000000/*.json`, `WRITEUP.mdx`. Corpus size actually
indexed was 1,000,000 documents, not the originally hoped-for 10M — the shared
benchmark machine had roughly 20 GiB free while six other experiments were
also running, and Meilisearch's own index (7.4 GiB at 1M docs) made a larger
corpus infeasible without holding three simultaneous indexes, which the disk
does not support.

## Decision

Use tuned Postgres FTS (stored generated `tsvector` + GIN index) as the
default full-text solution while both of these hold:

- the corpus is on the order of a few million rows or fewer (we directly
  measured 1M; we did not measure the point of failure between 1M and "a few
  million" and are not claiming one), and
- the product does not need typo/fuzzy/prefix tolerance, and query load is not
  dominated by very high-frequency terms (a term matching a large fraction of
  the table, ranked with `ts_rank_cd`, is the one place Postgres was
  measurably worse — p99 1.49s at 1M rows for a term hitting ~5% of the
  corpus, versus 20-32ms for Elasticsearch and Meilisearch).

Move to Elasticsearch once either condition breaks: the product needs typo
tolerance (Postgres FTS has none, full stop — this is a capability gap, not a
slow number) or common-term query load becomes a real fraction of traffic.
Meilisearch is a viable alternative to Elasticsearch on latency (it was
fastest of the three on every measured shape) but its 36-minute build time and
7.4 GiB index at 1M docs (vs. Postgres's 1.83 GiB and Elasticsearch's 456 MiB)
make it a worse fit for write-heavy or reindex-heavy workloads.

## Alternatives Considered

- **Always use Elasticsearch regardless of scale.** Rejected as the thing this
  ADR exists to correct — it is not free, and at the corpus size we measured
  it bought no relevance advantage (all three engines hit recall@10/MRR@10 of
  1.0 on every judged shape) and only a modest latency advantage on the shapes
  Postgres already handles well.
- **Always use Meilisearch.** Rejected as the default for anything
  write-heavy: it was the fastest reader here but by far the slowest and
  heaviest to build (36 min / 7.4 GiB at 1M rows vs. Postgres's 6 min / 1.8
  GiB), which matters for reindex cadence and disk budget, not just query
  latency.
- **Measure at 10M documents as originally planned.** Rejected for this run:
  the shared machine did not have the disk to hold even one 10M-document
  Meilisearch index alongside the other two engines being torn down and
  rebuilt in sequence, given ~20 GiB free while six unrelated experiments
  shared the box. Documented as a real deviation rather than silently
  downscoped.
- **Skip relevance measurement (a common shortcut in these bakeoffs).**
  Rejected — an unjudged relevance number is worse than none. We built a
  judgement set by construction instead (planted tokens, ground truth known by
  the generator, asserted by `corpus_test.go`), and explicitly marked the
  `common` query shape NOT MEASURED where the judgement set doesn't produce a
  meaningful top-10 cutoff.

## Consequences

- Teams under the stated threshold can defer standing up Elasticsearch
  entirely, avoiding a second datastore, JVM heap tuning, and dual-write
  consistency concerns — at the cost of accepting no typo tolerance and a
  slow path for very common terms unless they add their own mitigation
  (e.g. capping/short-circuiting common-term queries, a separate covering
  index, or application-level caching of hot queries).
- This decision is explicitly bounded at 1,000,000 documents on a single M1
  Pro node; it does not establish where between 1M and "a few million" Postgres
  actually degrades, nor whether the crossover point is different on real
  natural-language text with realistic term-frequency skew instead of this
  repo's synthetic Zipfian corpus. A team approaching this scale should re-run
  `make bench N=<their real scale>` against their own schema before trusting
  this threshold, not extrapolate from this line.
- Nothing here says anything about multi-node behavior, replica failover, or
  horizontal read scaling — a large part of the real argument for
  Elasticsearch in production is operational (rolling upgrades, replica
  failover, cross-cluster search), and this bakeoff, being single-node
  throughout, cannot speak to it either way.
- If the product later needs typo tolerance, this decision is void
  immediately regardless of corpus size — that is a capability gap for
  Postgres FTS, not a performance one, and no amount of tuning closes it.
