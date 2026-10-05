#!/bin/sh
# Validate release versions and package the chart with its license.
set -eu

chart=charts/healthchecks-kubernetes
metadata=$(helm show chart "$chart")
version=$(printf '%s\n' "$metadata" | awk '$1 == "version:" {gsub(/"/, "", $2); print $2}')
app_version=$(printf '%s\n' "$metadata" | awk '$1 == "appVersion:" {gsub(/"/, "", $2); print $2}')
if [ -z "$version" ] || [ "$version" != "$app_version" ]; then
    echo 'Chart version and appVersion must match' >&2
    exit 1
fi
if [ -n "${RELEASE_TAG:-}" ] && [ "$RELEASE_TAG" != "v$version" ]; then
    echo "Release tag must be v$version to match the chart" >&2
    exit 1
fi

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT HUP INT TERM
cp -R "$chart" "$staging/healthchecks-kubernetes"
cp LICENSE "$staging/healthchecks-kubernetes/LICENSE"
mkdir -p bin/charts
helm package "$staging/healthchecks-kubernetes" --destination bin/charts
package="bin/charts/healthchecks-kubernetes-$version.tgz"
helm template release "$package" --set config.cluster=ci \
    --set credentials.existingSecret=healthchecks > "$staging/rendered.yaml"
if ! grep -Fq "image: \"ghcr.io/felixtriller/healthchecks-kubernetes:$version\"" "$staging/rendered.yaml"; then
    echo 'Default image tag must match the chart version' >&2
    exit 1
fi
