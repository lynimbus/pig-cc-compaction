#!/bin/sh
# The Go module lives in extensions/pig-cc-compaction, where a Package puts an
# extension. This wrapper keeps `sh test.sh` working from the package root.
set -eu
exec sh "$(dirname "$0")/extensions/pig-cc-compaction/test.sh"
