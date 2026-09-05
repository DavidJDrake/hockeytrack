REGION      ?= us-east-1
ACCOUNT_ID  ?= $(shell aws sts get-caller-identity --query Account --output text)
REPO        := hockeytrack
TAG         ?= $(shell git rev-parse --short HEAD)
IMAGE       := $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com/$(REPO):$(TAG)

.PHONY: test vuln build push deploy site

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

# Upload the static website (everything except data/, which schedule-sync
# owns) and expire the CloudFront cache.
site:
	aws s3 sync site/ s3://$$(cd terraform && terraform output -raw site_bucket)/ --exclude 'data/*' --exclude 'assets/*' --delete --cache-control 'public, max-age=300' --region $(REGION)
	aws s3 sync site/assets/ s3://$$(cd terraform && terraform output -raw site_bucket)/assets/ --exclude 'fonts/*' --delete --cache-control 'public, max-age=86400' --region $(REGION)
	aws s3 sync site/assets/fonts/ s3://$$(cd terraform && terraform output -raw site_bucket)/assets/fonts/ --delete --cache-control 'public, max-age=31536000, immutable' --content-type 'font/woff2' --region $(REGION)
	aws cloudfront create-invalidation --distribution-id $$(cd terraform && terraform output -raw site_distribution_id) --paths '/*' --query 'Invalidation.Id' --output text
