FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /healthchecks-kubernetes ./cmd/healthchecks-kubernetes

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /healthchecks-kubernetes /healthchecks-kubernetes
COPY LICENSE /usr/share/licenses/healthchecks-kubernetes/LICENSE
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/healthchecks-kubernetes"]
