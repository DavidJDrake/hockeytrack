REGION      ?= us-east-1
ACCOUNT_ID  ?= $(shell aws sts get-caller-identity --query Account --output text)
REPO        := hockeytrack
TAG         ?= $(shell git rev-parse --short HEAD)
IMAGE       := $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com/$(REPO):$(TAG)

.PHONY: test build push deploy

test:
	go test ./...

build: test
	docker build --platform linux/arm64 -t $(IMAGE) .

push: build
	aws ecr get-login-password --region $(REGION) | docker login --username AWS --password-stdin $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com
	docker push $(IMAGE)

deploy: push
	cd terraform && terraform apply -var="image_tag=$(TAG)" -auto-approve
