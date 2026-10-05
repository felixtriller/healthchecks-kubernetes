.PHONY: test build lint chart

test:
	go test -race ./...

build:
	go build -trimpath -o bin/healthchecks-kubernetes ./cmd/healthchecks-kubernetes

lint:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)"

chart:
	helm lint charts/healthchecks-kubernetes --set config.cluster=ci --set credentials.existingSecret=healthchecks
	helm template healthchecks charts/healthchecks-kubernetes --set config.cluster=ci --set credentials.existingSecret=healthchecks > /dev/null
