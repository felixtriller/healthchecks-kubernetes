FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY scripts/third-party-licenses.sh scripts/
RUN sh scripts/third-party-licenses.sh > /THIRD_PARTY_LICENSES
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /healthchecks-kubernetes ./cmd/healthchecks-kubernetes

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /healthchecks-kubernetes /healthchecks-kubernetes
COPY LICENSE /usr/share/licenses/healthchecks-kubernetes/
COPY --from=build /THIRD_PARTY_LICENSES /usr/share/licenses/healthchecks-kubernetes/
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/healthchecks-kubernetes"]
