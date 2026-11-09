#!/usr/bin/env bash
# Regenerates the "Linked modules" section of THIRD_PARTY_NOTICES.md from go-licenses and fails on
# any copyleft license in the binary's module graph (G4). Everything above the generated marker in
# the notices file is hand-maintained (lifted code) and is left untouched.
set -euo pipefail
GO_LICENSES=${GO_LICENSES:-go-licenses}
GO=${GO:-go}
NOTICES=THIRD_PARTY_NOTICES.md
MARKER='<!-- generated: linked modules (scripts/third-party-notices.sh) -->'

if ! $GO list ./cmd/tollbooth >/dev/null 2>&1; then
  echo "third-party-notices: no ./cmd/tollbooth package yet; nothing linked, nothing to generate"
  exit 0
fi

# Deny list from docs/SPEC.md, Licensing and attribution.
$GO_LICENSES check ./cmd/tollbooth --disallowed_types=forbidden,restricted \
  --ignore github.com/blakegolliher/tollbooth
report=$($GO_LICENSES report ./cmd/tollbooth --ignore github.com/blakegolliher/tollbooth 2>/dev/null | sort)

tmp=$(mktemp)
sed "/^${MARKER//\//\\/}$/,\$d" "$NOTICES" > "$tmp"
{
  echo "$MARKER"
  echo
  echo "| Module | License | URL |"
  echo "|---|---|---|"
  echo "$report" | awk -F, '{printf "| `%s` | %s | %s |\n", $1, $3, $2}'
} >> "$tmp"
mv "$tmp" "$NOTICES"
echo "third-party-notices: regenerated $NOTICES"
