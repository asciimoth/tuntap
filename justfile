set shell := ["bash", "-euo", "pipefail", "-c"]
set dotenv-load := true

typos:
  typos

check: tidy typos fmt vet test fuzz

fuzz:
	#!/usr/bin/env bash
	if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
	  echo "Skipping fuzz tests in GitHub Actions."
	  exit 0
	fi
	fuzz_time="${FUZZ_TIME:-1m}"
	mapfile -t fuzz_targets < <(go test . -list '^Fuzz' | sed -n '/^Fuzz/p')
	if (( ${#fuzz_targets[@]} == 0 )); then
	  echo "No fuzz targets are available on this platform."
	  exit 0
	fi
	for fuzz_target in "${fuzz_targets[@]}"; do
	  go test . -run '^$' -fuzz "^${fuzz_target}$" -fuzztime "$fuzz_time"
	done

test:
	go test ./... --race -count=1

vet:
	go vet ./...

tidy:
	go mod tidy

# lint:
#   golangci-lint run ./...

fmt:
  golangci-lint fmt ./...
