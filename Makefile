REGISTRY?=docker.io
REPOSITORY?=gmehta3/network-migration-sidecar
VERSION?=latest
IMAGE=$(REGISTRY)/$(REPOSITORY):$(VERSION)

image:
	docker buildx build --load --platform linux/amd64 -t $(IMAGE) .

push:
	docker push $(IMAGE)
	
default: image push