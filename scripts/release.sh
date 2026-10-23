#!/bin/sh
set -eu
version=${1:-}
printf '%s\n' "$version" | LC_ALL=C grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$' || { echo 'Usage: scripts/release.sh vX.Y.Z' >&2; exit 1; }
cd "$(dirname "$0")/.."
mkdir -p dist
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
trap 'exit 1' HUP INT TERM
# Include the notices for all modules linked into the release, plus Go's runtime.
{
    printf 'Go runtime and standard library\n\n'
    cat "$(go env GOROOT)/LICENSE"
    go list -m -f '{{if not .Main}}{{.Path}} {{.Dir}}{{end}}' all | while read -r module directory; do
        [ -n "$module" ] || continue
        [ -f "$directory/LICENSE" ] || { printf 'Missing license for %s\n' "$module" >&2; exit 1; }
        printf '\n\n%s\n\n' "$module"
        cat "$directory/LICENSE"
    done
} > "$work/THIRD_PARTY_NOTICES.txt"
for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$work/bndr" .
    tar -czf "dist/bndr_linux_$arch.tar.gz" -C "$work" bndr THIRD_PARTY_NOTICES.txt -C "$PWD" README.md
done
cp install.sh dist/install.sh
(cd dist && sha256sum bndr_linux_amd64.tar.gz bndr_linux_arm64.tar.gz install.sh > SHA256SUMS)
printf 'Release artifacts for %s are in dist/\n' "$version"
