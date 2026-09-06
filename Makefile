REGION      ?= us-east-1
ACCOUNT_ID  ?= $(shell aws sts get-caller-identity --query Account --output text)
REPO        := hockeytrack
TAG         ?= $(shell git rev-parse --short HEAD)
IMAGE       := $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com/$(REPO):$(TAG)

# Terraform ships as a snap and refuses to run without a *writable*
# XDG_RUNTIME_DIR. A login shell usually points it at /run/user/$(shell id -u),
# which systemd-logind may never have created and which the user often cannot
# create either, so `?=` is not enough: an already-set but unusable value has to
# be replaced, not deferred to. Every recipe that reads a terraform output
# needs this — `backfill`, `site`, `deploy`, `replay` and `livefire`.
# Only replaces a value that does not work, so a healthy desktop or CI session
# keeps its own and a fork of this repo is unaffected.
export XDG_RUNTIME_DIR := $(shell test -w "$${XDG_RUNTIME_DIR}" 2>/dev/null && echo "$${XDG_RUNTIME_DIR}" || (mkdir -p "$(HOME)/.cache/xdg-runtime" && chmod 700 "$(HOME)/.cache/xdg-runtime" && echo "$(HOME)/.cache/xdg-runtime"))

SEASONS     ?= all
RPS         ?= 3

.PHONY: test vuln build push deploy site backfill og replay golden golden-update livefire

test: vuln
	go test ./...

# Fails on any known vulnerability reachable from this module's code.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# The final image is flattened to a single layer (export/import): this host's
# Docker daemon pushes layered images whose shared base blobs Lambda cannot
# apply (Runtime.InvalidEntrypoint/ProcessSpawnFailed), while a fresh
# single-layer image always works. Costs layer dedupe (~12MB/version), buys
# a deploy that cannot reference a poisoned blob.
build: test
	docker build --platform linux/amd64 -t $(IMAGE)-layered .
	docker rm -f hockeytrack-flatten 2>/dev/null || true
	docker create --name hockeytrack-flatten $(IMAGE)-layered
	docker export hockeytrack-flatten | docker import --change 'ENTRYPOINT ["/ingestor"]' --change 'USER 65532' - $(IMAGE)
	docker rm hockeytrack-flatten

push: build
	aws ecr get-login-password --region $(REGION) | docker login --username AWS --password-stdin $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com
	docker push $(IMAGE)

deploy: push
	cd terraform && terraform apply -var="image_tag=$(TAG)" -auto-approve

# Archive past seasons' final feeds into the raw bucket. Local, S3-only,
# resumable; see README "Backfilling history".
backfill:
	AWS_REGION=$(REGION) go run ./cmd/backfill -bucket $$(cd terraform && terraform output -raw raw_bucket) -seasons $(SEASONS) -rps $(RPS)

# Re-render the home page's Open Graph card from docs/og/home.html with the
# countdown as of now (needs google-chrome). The digits in the card are a
# snapshot, so run this before `make site` when a fresh preview matters.
og:
	./docs/og/render.sh

# Upload the static website (everything except data/, which schedule-sync
# owns) and expire the CloudFront cache.
site:
	aws s3 sync site/ s3://$$(cd terraform && terraform output -raw site_bucket)/ --exclude 'data/*' --exclude 'assets/*' --delete --cache-control 'public, max-age=300' --region $(REGION)
	aws s3 sync site/assets/ s3://$$(cd terraform && terraform output -raw site_bucket)/assets/ --exclude 'fonts/*' --delete --cache-control 'public, max-age=86400' --region $(REGION)
	aws s3 sync site/assets/fonts/ s3://$$(cd terraform && terraform output -raw site_bucket)/assets/fonts/ --delete --cache-control 'public, max-age=31536000, immutable' --content-type 'font/woff2' --region $(REGION)
	aws cloudfront create-invalidation --distribution-id $$(cd terraform && terraform output -raw site_distribution_id) --paths '/*' --query 'Invalidation.Id' --output text

# Replay one archived game through the poller offline, printing every event
# it publishes. GAME is an NHL game id; INTERVAL groups plays into snapshots
# of that much game time (0 for one snapshot per play).
GAME     ?=
INTERVAL ?= 30s
SPEED    ?= 60

replay:
	@test -n "$(GAME)" || { echo "usage: make replay GAME=2024021299 [INTERVAL=30s]"; exit 2; }
	AWS_REGION=$(REGION) HOCKEYTRACK_RAW_BUCKET=$$(cd terraform && terraform output -raw raw_bucket) \
		go run ./cmd/replay -game $(GAME) -interval $(INTERVAL)

# The golden event-stream regression suite. Runs offline from checked-in
# fixtures; needs no AWS credentials.
golden:
	go test ./internal/synth/ -run TestGolden

# Rewrite the golden files. Separate from `golden` so a regeneration is
# always deliberate: review the diff before committing it.
golden-update:
	go test ./internal/synth/ -run TestGolden -update -v

# Replay a game onto the REAL event bus under the synthetic source, which no
# notification rule matches. Add -as-poller by hand if you specifically want
# to test the notification path; that can send email and SMS.
livefire:
	@test -n "$(GAME)" || { echo "usage: make livefire GAME=2024021299 [SPEED=60]"; exit 2; }
	AWS_REGION=$(REGION) HOCKEYTRACK_RAW_BUCKET=$$(cd terraform && terraform output -raw raw_bucket) \
		go run ./cmd/livefire -game $(GAME) -speed $(SPEED) -interval $(INTERVAL)
