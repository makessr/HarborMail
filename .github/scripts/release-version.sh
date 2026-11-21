#!/usr/bin/env bash
set -euo pipefail

git tag --points-at HEAD --sort=-version:refname |
  awk '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ && !seen++ {print}'
