#!/usr/bin/env bash
set -euo pipefail

# Sets up the project's git hooks by pointing core.hooksPath to the scripts directory.

repo_root=$(git rev-parse --show-toplevel)
hooks_dir="$repo_root/scripts"

git config core.hooksPath "$hooks_dir"
echo "Git hooks configured to use $hooks_dir"
