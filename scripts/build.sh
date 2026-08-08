#!/usr/bin/env bash
set -euo pipefail

go mod tidy
go build -o bin/ingest ./cmd/ingest
go build -o bin/bot ./cmd/bot
echo "built bin/ingest and bin/bot"
