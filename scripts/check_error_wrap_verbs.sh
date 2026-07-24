#!/usr/bin/env bash
#
# Forbid the %w verb inside teranode errors.New* format strings.
#
# The teranode errors package (errors/errors.go) extracts a trailing error
# argument as the wrapped error BEFORE calling fmt.Errorf, so any %w left in the
# format string is orphaned: it renders as "%!w(MISSING)", or (when no other
# params remain) the literal "%w" survives unformatted. The wrapped error is
# rendered by Error() via " -> " chaining, so %w is always wrong here.
#
# Pass the causing error as the final argument and drop the verb:
#   errors.NewStorageError("failed to read block %s: %w", hash, err)  // WRONG
#   errors.NewStorageError("failed to read block %s", hash, err)      // RIGHT
#
# The errors package itself is excluded: its own unit tests deliberately build
# %w format strings to exercise the defensive stripping (see issue #1332), and
# that path is already exempt from these style rules in .golangci.json.
set -euo pipefail

cd "$(dirname "$0")/.."

# Match errors.New / errors.New<Type>Error( ... %w on a single line. Two forms
# escape a line-based grep (a %w on a format-string continuation line, or a
# pre-formatted %%w through fmt.Sprintf); those are additionally caught at
# runtime by the defensive strip in errors.New.
matches=$(grep -rnE 'errors\.New[A-Za-z]*\([^)]*%w' \
  --include='*.go' --exclude-dir=vendor --exclude-dir=errors . || true)

if [ -n "$matches" ]; then
  echo "ERROR: %w found inside errors.New* format string(s)."
  echo "The trailing error argument is extracted as the wrapped error before"
  echo "formatting, so %w renders as %!w(MISSING) or survives literally."
  echo "Drop the verb; pass the error as the final argument. Error() renders"
  echo "the chain via \" -> \"."
  echo
  echo "$matches"
  exit 1
fi

echo "OK: no %w verbs in errors.New* format strings."
