N ?= 500000
REPS ?= 4
C ?= 8
OUT = results/n$(N)
# host-wide, owner-checked benchmark mutex shared with the other experiments
LOCK ?= /tmp/expbrief/benchlock.sh
ME ?= search-engine-bakeoff

# `make bench` runs the whole three-way matrix at corpus size N, one engine at
# a time: bring the engine up, load, measure, record RSS and disk, tear it
# down. Three indexes of this corpus do not fit on the test machine's disk.
# The load phase runs unlocked; the shared benchmark lock is held only while
# queries are in flight, and released explicitly by its owner. Each arm takes
# the lock separately because the three indexes cannot coexist on disk.
.PHONY: bench
bench: build
	@mkdir -p $(OUT)
	$(MAKE) arm PROFILE=pg ENGINE=postgres SVC=postgres DATA=/var/lib/postgresql/data
	$(MAKE) arm PROFILE=es ENGINE=elasticsearch SVC=elasticsearch DATA=/usr/share/elasticsearch/data
	$(MAKE) arm PROFILE=ms ENGINE=meilisearch SVC=meilisearch DATA=/meili_data
	@echo "results in $(OUT)"

.PHONY: build
build:
	go build -o /tmp/bakeoff ./cmd/bakeoff

# one arm: PROFILE is the compose profile, ENGINE the harness driver
.PHONY: arm
arm:
	@mkdir -p $(OUT)
	docker compose --profile $(PROFILE) up -d --wait
	/tmp/bakeoff -engine $(ENGINE) -n $(N) -phase load -state $(OUT)/$(ENGINE).build.json 2>&1 | tee $(OUT)/$(ENGINE).log
	@ps -eo args | grep -Ei 'k6|vegeta|wrk|bench' | grep -v grep | tee -a $(OUT)/$(ENGINE).log || true
	$(LOCK) acquire $(ME) && { /tmp/bakeoff -engine $(ENGINE) -n $(N) -c $(C) -reps $(REPS) -phase query -state $(OUT)/$(ENGINE).build.json -out $(OUT)/$(ENGINE).json 2>&1 | tee -a $(OUT)/$(ENGINE).log; s=$$?; $(LOCK) release $(ME); exit $$s; }
	docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.CPUPerc}}' | tee $(OUT)/$(ENGINE).rss.txt
	docker compose exec -T $(SVC) du -sk $(DATA) | tee $(OUT)/$(ENGINE).du.txt
	df -h /System/Volumes/Data | tee -a $(OUT)/$(ENGINE).log
	docker compose --profile $(PROFILE) down -v

.PHONY: test lint clean
test:
	go test ./...
lint:
	golangci-lint run
clean:
	docker compose --profile pg --profile es --profile ms down -v
