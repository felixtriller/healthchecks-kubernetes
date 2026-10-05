.PHONY: test build lint chart licenses

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

licenses:
	mkdir -p bin
	sh scripts/third-party-licenses.sh > bin/THIRD_PARTY_LICENSES
