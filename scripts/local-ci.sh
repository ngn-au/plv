#!/usr/bin/env bash
# Reusable local CI — run BEFORE pushing.
#   scripts/local-ci.sh fast     # gofmt + vet + race tests + govulncheck
#   scripts/local-ci.sh codeql   # CodeQL security-and-quality (Go) in a linux/amd64 container
#   scripts/local-ci.sh act      # run the GitHub Actions workflows via act (Docker)
set -euo pipefail
cd "$(dirname "$0")/.."

fast() {
  gofmt -l . | (! grep .) && echo "gofmt ok"
  go vet ./...
  go test -race ./...
  go run golang.org/x/vuln/cmd/govulncheck@latest ./...
}

codeql() {
  # Intel-only macOS bundle + Apple Silicon = no native run; use Docker (linux/amd64).
  local bundle=/tmp/codeql-linux
  # The codeql-bundle tarball extracts to a top-level `codeql/` dir holding the `codeql`
  # binary; we rename that dir to $bundle, so the binary lands directly at $bundle/codeql.
  if [ ! -x "$bundle/codeql" ]; then
    curl -sL https://github.com/github/codeql-action/releases/latest/download/codeql-bundle-linux64.tar.gz \
      | tar xz -C "$(dirname "$bundle")" && mv "$(dirname "$bundle")/codeql" "$bundle" 2>/dev/null || true
  fi
  docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src -v "$bundle":/cq \
    golang:1.26.3 bash -c '
      /cq/codeql database create /tmp/db --language=go --source-root=/src --overwrite >/tmp/cq.log 2>&1 || { tail -20 /tmp/cq.log; exit 1; }
      /cq/codeql database analyze /tmp/db codeql/go-queries:codeql-suites/go-security-and-quality.qls \
        --format=sarif-latest --output=/src/codeql-go.sarif --threads=0 >>/tmp/cq.log 2>&1 || { tail -20 /tmp/cq.log; exit 1; }'
  python3 - <<'PY'
import json
d=json.load(open("codeql-go.sarif"))
res=d["runs"][0].get("results",[])
print(f"CodeQL results: {len(res)}")
for r in res:
    loc=r["locations"][0]["physicalLocation"]
    print(f"  {r['ruleId']}  {loc['artifactLocation']['uri']}:{loc['region'].get('startLine')}  {r['message']['text'][:80]}")
PY
}

act_ci() { act -W .github/workflows/ci.yml --container-architecture linux/amd64 "$@"; }

case "${1:-fast}" in
  fast) fast ;;
  codeql) codeql ;;
  act) shift; act_ci "$@" ;;
  *) echo "usage: $0 {fast|codeql|act}"; exit 1 ;;
esac
