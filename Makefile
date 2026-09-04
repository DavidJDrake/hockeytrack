REGION      ?= us-east-1
ACCOUNT_ID  ?= $(shell aws sts get-caller-identity --query Account --output text)
REPO        := hockeytrack
TAG         ?= $(shell git rev-parse --short HEAD)
IMAGE       := $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com/$(REPO):$(TAG)

.PHONY: test build push deploy site

test:
	go test ./...

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
	aws s3 sync site/ s3://$$(cd terraform && terraform output -raw site_bucket)/ --exclude 'data/*' --delete --region $(REGION)
	aws cloudfront create-invalidation --distribution-id $$(cd terraform && terraform output -raw site_distribution_id) --paths '/*' --query 'Invalidation.Id' --output text
