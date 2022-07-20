SHELL :=/bin/bash

REGISTRY ?= quay.io/open-cluster-management-hub-of-hubs
IMAGE_TAG ?= latest

.PHONY: vendor			##download all third party libraries and puts them inside vendor directory
vendor:
	@go mod vendor

all: build
.PHONY: all

build:
	go build -o hub-of-hubs-addon-controller cmd/main.go

build-image: vendor
	docker build -t ${REGISTRY}/hub-of-hubs-addon-controller:${IMAGE_TAG} . -f Dockerfile

push-image:
	docker push ${REGISTRY}/hub-of-hubs-addon-controller:${IMAGE_TAG}


