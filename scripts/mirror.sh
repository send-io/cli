#!/bin/sh
# Builds the tree of the public send-io/cli repo from this monorepo: cli/ at
# the root, plus the two local modules its go.mod replaces (sdks/go and
# packages/email-renderer) under third_party/, with go.mod pointing there, so
# the public repo builds and releases on its own. Tests and examples of the
# two modules are left out; the Go SDK keeps its LICENSE and NOTICE.
#
#   cli/scripts/mirror.sh origin/develop /path/to/send-io-cli-checkout
#
# It replaces every tracked file in the target (a clone of send-io/cli), then
# checks that the result builds and passes its tests. Commit and push there.
set -eu
ref=${1:?git ref, e.g. origin/develop}
out=${2:?target directory (a clone of send-io/cli)}
root=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

git -C "$root" archive "$ref" cli sdks/go packages/email-renderer | tar -x -C "$tmp"
find "$tmp/sdks/go" "$tmp/packages/email-renderer" -name '*_test.go' -delete
rm -rf "$tmp/sdks/go/examples"

git -C "$out" rm -rq --ignore-unmatch .
git -C "$out" clean -fdq
cp -R "$tmp/cli/." "$out/"
mkdir -p "$out/third_party"
cp -R "$tmp/sdks/go" "$out/third_party/sendio-go"
cp -R "$tmp/packages/email-renderer" "$out/third_party/email-renderer"
sed -e 's#=> \.\./sdks/go#=> ./third_party/sendio-go#' \
    -e 's#=> \.\./packages/email-renderer#=> ./third_party/email-renderer#' \
    "$tmp/cli/go.mod" > "$out/go.mod"
grep -q '=> ./third_party/sendio-go' "$out/go.mod" && grep -q '=> ./third_party/email-renderer' "$out/go.mod" ||
  { echo "mirror: go.mod replace lines not found; did cli/go.mod change?" >&2; exit 1; }

(cd "$out" && go build ./... && go test ./... >/dev/null)
git -C "$out" add -A
echo "mirror: $out matches $ref ($(git -C "$root" rev-parse --short "$ref")), builds and passes its tests"
