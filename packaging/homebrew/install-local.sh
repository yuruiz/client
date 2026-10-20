#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
package_name="git-remote-keybase-standalone"
package_version="${1:-$(cat "$script_dir/VERSION")}"
release_dir="${KEYBASE_GIT_RELEASE_DIR:-$HOME/.local/share/keybase-git-standalone/releases}"
tap_name="${KEYBASE_GIT_HOMEBREW_TAP:-local/keybase-git}"
brew_bin="${HOMEBREW_BREW_FILE:-$(command -v brew)}"

if [[ ! "$tap_name" =~ ^[a-z0-9_-]+/[a-z0-9_-]+$ ]]; then
  echo "Invalid tap name: $tap_name" >&2
  exit 1
fi

python3 "$script_dir/release.py" --version "$package_version" --output "$release_dir"
tap_dir="$("$brew_bin" --repository "$tap_name")"
if [[ ! -d "$tap_dir" ]]; then
  HOMEBREW_DEVELOPER=1 "$brew_bin" tap-new --no-git "$tap_name"
fi
mkdir -p "$tap_dir/Formula"
cp "$release_dir/$package_version/$package_name.rb" "$tap_dir/Formula/$package_name.rb"

formula="$tap_name/$package_name"
if "$brew_bin" list --formula "$formula" >/dev/null 2>&1; then
  # upgrade retains the original bottle-build setting; --build-bottle itself
  # is an install-only flag.
  "$brew_bin" upgrade --build-from-source "$formula"
else
  "$brew_bin" install --build-bottle "$formula"
fi
"$brew_bin" test "$formula"
echo "Installed: $("$brew_bin" --prefix)/bin/git-remote-keybase"
