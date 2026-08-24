# search-engine-bakeoff

At what corpus size and query shape does PostgreSQL full-text search stop being
enough? "Just use Elasticsearch" is advice that costs teams an ops budget they
may not need, so this repo measures the three usual candidates on the same
seeded corpus with the same judged queries:

- **PostgreSQL 17** FTS — stored generated `tsvector` column + GIN index
- **Elasticsearch 8.19.5** — single node, 2 GB heap
- **Meilisearch v1.24** — single node

Measured per engine per query shape: recall@10, MRR@10, p50/p95/p99, throughput,
index build wall time, index size on disk, and steady-state container RSS.

## Headline

<!-- HEADLINE -->

## Relevance is judged, not guessed

The corpus is generated from a fixed seed and marker tokens are **planted into
known document ids**, so the relevant set for each query is known by
construction rather than assumed:

| shape    | query                                       | ground truth                                                                    |
| -------- | ------------------------------------------- | ------------------------------------------------------------------------------- |
| `rare`   | one planted token                           | the 10 docs carrying it                                                         |
| `and`    | two planted tokens                          | the 10 docs carrying both (50 more carry each one alone)                        |
| `phrase` | two planted tokens, adjacent                | the 10 docs where they are adjacent (50 more carry both, far apart)             |
| `typo`   | the rare token with one character corrupted | the same 10 docs                                                                |
| `common` | a token in 5% of the corpus                 | **NOT MEASURED** — tens of thousands of docs qualify, so recall@10 says nothing |

Rare tokens differ from each other in a four-letter block, so the typo query is
edit distance 1 from its own planted docs and at least 3 from every other
planted set: a fuzzy matcher cannot be accidentally right. `cmd/bakeoff/corpus_test.go`
asserts all of this against the generated text.

Postgres FTS has no typo tolerance out of the box. That arm reports
`supported: false` rather than a latency number — it is a capability gap, not a
slow query, and it is not scored as if it were the same query.

## What was tuned

- **Postgres**: `shared_buffers=2GB`, `work_mem=64MB`, `maintenance_work_mem=1GB`,
  `effective_cache_size=6GB`, `random_page_cost=1.1`. Data loaded with `COPY`,
  the GIN index created _after_ the load, then `ANALYZE`. Ranking is
  `ts_rank_cd`, queries built with `plainto_tsquery` / `phraseto_tsquery`.
- **Elasticsearch**: 1 shard, 0 replicas, `refresh_interval` disabled during the
  bulk load and restored afterwards, force-merged to one segment,
  `ES_JAVA_OPTS=-Xms2g -Xmx2g`, security and ML off.
- **Meilisearch**: `MEILI_ENV=production`, searchable attributes restricted to
  `title` and `body`, documents pushed in 20k batches, index task awaited.

All three are single node. None of them is a cluster, and nothing here says
anything about how they scale horizontally.

## Machine

Apple M1 Pro, 8 cores, 16 GB RAM, macOS (Darwin 25.6.0). Docker via OrbStack
29.4.0, Linux VM capped at 8 CPU / 8 GB RAM. Load generator (the Go harness)
runs on the host and targets containers over `localhost`. Go 1.26.

## Reproduce from cold

```bash
git clone https://github.com/dhiazfathra/search-engine-bakeoff
cd search-engine-bakeoff
make bench N=1000000        # ~40 min: three engines, one at a time
```

`make bench` brings each engine up on its own compose profile, takes the
host-wide benchmark lock, loads the corpus, measures, records RSS and on-disk
size, releases the lock and tears the engine down before the next one starts.
Index build time is one of the reported numbers, so the lock covers the load as
well as the queries. Three indexes of a million documents do not coexist
comfortably on the test machine's disk, which is why the arms are sequential —
and why the lock is taken three times rather than once across the matrix.

`make test` runs the judgement-set assertions, `make lint` runs golangci-lint,
`make clean` removes any leftover stack.

## Layout

```
cmd/bakeoff/corpus.go   seeded generator + planted judgement set
cmd/bakeoff/engines.go  the three drivers
cmd/bakeoff/main.go     load phase, query phase, percentiles
results/n<N>/           raw JSON, logs, RSS and du output per engine
docs/adr/0001-*.md      the decision
WRITEUP.mdx             the write-up
```
