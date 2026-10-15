#!/usr/bin/env bash
# Copies the generated OTLP protobuf types (trace, common, resource) from Grafana Tempo's
# pkg/tempopb (Apache-2.0, see internal/tempopb/LICENSE_APACHE2) into internal/tempopb.
#
# Only those three generated files and the licence are fetched, never other Tempo source: the
# clean-room rule in CONTRIBUTING.md allows pkg/tempopb only. Tempo's tags cannot be used as Go
# module versions (its go.mod path has no major-version suffix), hence the copy.
#
# Usage: hack/sync-tempopb.sh [tag] [destination dir]
#   tag  defaults to the latest GitHub release; destination defaults to internal/tempopb.
set -euo pipefail

repo=grafana/tempo
tag="${1:-$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | jq -r .tag_name)}"
dest="${2:-$(cd "$(dirname "$0")/.." && pwd)/internal/tempopb}"
module="github.com/querdian-dev/querdian/internal/tempopb"
raw="https://raw.githubusercontent.com/$repo/$tag/pkg/tempopb"

[ -n "$tag" ] && [ "$tag" != null ] || { echo "no tag" >&2; exit 1; }
commit=$(curl -fsSL "https://api.github.com/repos/$repo/commits/$tag" | jq -r .sha)

mkdir -p "$dest"
curl -fsSL "$raw/LICENSE_APACHE2" -o "$dest/LICENSE_APACHE2"
for p in common resource trace; do
  mkdir -p "$dest/$p/v1"
  curl -fsSL "$raw/$p/v1/$p.pb.go" |
    sed "s#\"github.com/grafana/tempo/pkg/tempopb/#\"$module/#" >"$dest/$p/v1/$p.pb.go"
done

# Fail on any remaining reference to Tempo packages: the copy must be self-contained.
if grep -rn 'grafana/tempo' "$dest" --include='*.go'; then
  echo "unexpected import of Tempo packages" >&2
  exit 1
fi

cat >"$dest/SOURCE" <<EOF
Copied from https://github.com/$repo (pkg/tempopb/{common,resource,trace}/v1, Apache-2.0)
tag:    $tag
commit: $commit
Update with hack/sync-tempopb.sh; do not edit the .pb.go files by hand.
EOF
echo "synced $tag ($commit) into $dest"
