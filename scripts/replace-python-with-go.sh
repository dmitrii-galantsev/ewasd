#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT=$(unset CDPATH; cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
XDG_DATA_HOME=${XDG_DATA_HOME:-"$HOME/.local/share"}
LEGACY_WORKSPACE=${EWASD_LEGACY_WORKSPACE:-"$XDG_DATA_HOME/ewasd"}
GO_STATE=${EWASD_HOME:-"$XDG_DATA_HOME/ewasd-v2"}
INSTALL_MODE=${EWASD_INSTALL_MODE:-nix}
BUILD_MODE=${EWASD_BUILD_MODE:-nix}
NIX_PROFILE=${EWASD_NIX_PROFILE:-}
mkdir -p -- "$GO_STATE"
chmod 0700 "$GO_STATE"
TEMP_PARENT="$GO_STATE/.installer-tmp"
mkdir -p -- "$TEMP_PARENT"
chmod 0700 "$TEMP_PARENT"
TEMP_ROOT=$(mktemp -d "$TEMP_PARENT/ewasd-go-replacement.XXXXXX")

cleanup() {
  rm -rf -- "$TEMP_ROOT"
}
trap cleanup EXIT INT TERM

say() {
  printf '==> %s\n' "$*"
}

find_nix() {
  if command -v nix >/dev/null 2>&1; then
    command -v nix
    return
  fi
  local candidate
  for candidate in \
    "$HOME/.nix-profile/bin/nix" \
    "$HOME/.local/state/nix/profile/bin/nix" \
    /nix/var/nix/profiles/default/bin/nix \
    /nix/store/*-nix-*/bin/nix; do
    if [ -x "$candidate" ]; then
      printf '%s\n' "$candidate"
      return
    fi
  done
  return 1
}

resolve_installed() {
  local candidate
  if [ -n "$NIX_PROFILE" ]; then
    candidate="$NIX_PROFILE/bin/ewasd"
    [ -x "$candidate" ] && { printf '%s\n' "$candidate"; return; }
    return 0
  fi
  candidate="$HOME/.local/state/nix/profile/bin/ewasd"
  [ -x "$candidate" ] && { printf '%s\n' "$candidate"; return; }
  command -v ewasd 2>/dev/null || true
}

install_go() {
  if [ "$INSTALL_MODE" = nix ]; then
    say "activating the Go command before mutating legacy links"
    local profile_json had_python=false priority=5
    PROFILE_ARGS=()
    if [ -n "$NIX_PROFILE" ]; then
      PROFILE_ARGS=(--profile "$NIX_PROFILE")
    fi
    INSTALLED=$(resolve_installed)
    profile_json=$($NIX profile list "${PROFILE_ARGS[@]}" --json)
    if [ -x "$INSTALLED" ] && ! "$INSTALLED" --version 2>/dev/null | grep -q '^ewasd 1\.'; then
      had_python=true
    fi
    if [ ! -x "$INSTALLED" ] || ! cmp -s "$GO_BINARY" "$INSTALLED"; then
      if [ "$had_python" = true ] && printf '%s' "$profile_json" | grep -q '"ewasd":{'; then
        $NIX profile upgrade "${PROFILE_ARGS[@]}" ewasd \
          --override-flake github:dmitrii-galantsev/ewasd "$SOURCE_FLAKE" || true
      fi
      INSTALLED=$(resolve_installed)
      if [ ! -x "$INSTALLED" ] || ! cmp -s "$GO_BINARY" "$INSTALLED"; then
        say "installing the exact Go build at higher priority before removing Python"
        if [ "$had_python" = true ]; then
          priority=4
        elif printf '%s' "$profile_json" | grep -q 'packages\.[^"]*\.ewasd'; then
          # Same-version source refreshes need to coexist briefly with the
          # active Go element. A unique lower priority guarantees the exact new
          # build wins without a command-absence window.
          priority=$(( -1 * $(date +%s) ))
        fi
        $NIX profile add "${PROFILE_ARGS[@]}" "$SOURCE_FLAKE#ewasd" --priority "$priority"
        INSTALLED=$(resolve_installed)
        if [ ! -x "$INSTALLED" ] || ! cmp -s "$GO_BINARY" "$INSTALLED"; then
          printf 'error: newly built Go package did not become active\n' >&2
          exit 1
        fi
      fi
    fi
    if [ "$had_python" = true ]; then
      $NIX profile remove "${PROFILE_ARGS[@]}" ewasd
      INSTALLED=$(resolve_installed)
    fi
    local active_store profile_after old_store
    active_store=$(dirname -- "$(dirname -- "$(readlink -f -- "$INSTALLED")")")
    profile_after=$($NIX profile list "${PROFILE_ARGS[@]}" --json)
    while IFS= read -r old_store; do
      [ -n "$old_store" ] || continue
      if [ "$old_store" != "$active_store" ]; then
        $NIX profile remove "${PROFILE_ARGS[@]}" "$old_store"
      fi
    done < <(printf '%s' "$profile_after" | grep -oE '/nix/store/[a-z0-9]+-ewasd-[^" ]+' | sort -u)
    INSTALLED=$(resolve_installed)
  elif [ "$INSTALL_MODE" = copy ]; then
    INSTALLED=${EWASD_INSTALL_BIN:-"$HOME/.local/bin/ewasd"}
    mkdir -p -- "$(dirname -- "$INSTALLED")"
    local staged="$INSTALLED.new.$$"
    cp -- "$GO_BINARY" "$staged"
    chmod 0755 "$staged"
    mv -f -- "$staged" "$INSTALLED"
  else
    printf 'error: unsupported EWASD_INSTALL_MODE=%s\n' "$INSTALL_MODE" >&2
    exit 1
  fi
  if [ ! -x "$INSTALLED" ]; then
    printf 'error: installed ewasd binary not found after replacement\n' >&2
    exit 1
  fi
  INSTALLED_VERSION=$($INSTALLED --version)
  case "$INSTALLED_VERSION" in
    "ewasd 1."*) ;;
    *) printf 'error: installed command is not Go ewasd v1: %s\n' "$INSTALLED_VERSION" >&2; exit 1 ;;
  esac
}

NIX=""
if [ "$INSTALL_MODE" = nix ] && [ "$BUILD_MODE" != nix ]; then
  printf 'error: EWASD_INSTALL_MODE=nix requires EWASD_BUILD_MODE=nix\n' >&2
  exit 1
fi
if [ "$BUILD_MODE" = nix ] || [ "$INSTALL_MODE" = nix ]; then
  NIX=$(find_nix) || {
    printf 'error: Nix is required for the default replacement workflow\n' >&2
    exit 1
  }
fi

if [ "$BUILD_MODE" = nix ]; then
  # Never pass the whole working tree to `path:`. Nix copies path flakes into
  # the world-readable store, and this checkout intentionally contains local
  # ignored files. Build a durable allowlisted source containing Go/package
  # inputs only.
  SOURCE_TREE="$GO_STATE/legacy/installer-source-$(date -u +%Y%m%dT%H%M%SZ)-$$"
  mkdir -p -- "$SOURCE_TREE"
  (
    cd "$ROOT"
    tar -cf - flake.nix flake.lock go.mod cmd internal
  ) | tar -xf - -C "$SOURCE_TREE"
  SOURCE_FLAKE="path:$SOURCE_TREE"
  say "building the Go package from a sanitized Nix source"
  PACKAGE=$($NIX build --no-link --print-out-paths "$SOURCE_FLAKE#ewasd")
  GO_BINARY="$PACKAGE/bin/ewasd"
elif [ "$BUILD_MODE" = go ]; then
  say "building the Go binary directly for the isolated fixture/install"
  GO_BINARY="$TEMP_ROOT/ewasd"
  (cd "$ROOT" && go build -trimpath -o "$GO_BINARY" ./cmd/ewasd)
else
  printf 'error: unsupported EWASD_BUILD_MODE=%s\n' "$BUILD_MODE" >&2
  exit 1
fi

VERSION=$($GO_BINARY --version)
case "$VERSION" in
  "ewasd 1."*) ;;
  *)
    printf 'error: built binary is not the Go ewasd v1 release: %s\n' "$VERSION" >&2
    exit 1
    ;;
esac
say "built $VERSION"

install_go

SCAN_ARGS=()
SCAN_VALUE=${EWASD_SCAN_ROOTS:-"$HOME/git"}
IFS=: read -r -a SCAN_ROOTS <<< "$SCAN_VALUE"
for scan_root in "${SCAN_ROOTS[@]}"; do
  [ -n "$scan_root" ] || continue
  if [ -d "$scan_root" ]; then
    SCAN_ARGS+=(--scan-root "$scan_root")
  fi
done
if [ ${#SCAN_ARGS[@]} -eq 0 ]; then
  SCAN_ARGS+=(--scan-root "$ROOT")
fi

if [ -f "$LEGACY_WORKSPACE/editors.toml" ]; then
  say "previewing exact live links from the Python workspace"
  mkdir -p -- "$GO_STATE/legacy"
  EWASD_HOME="$GO_STATE" "$GO_BINARY" migrate-legacy \
    --workspace "$LEGACY_WORKSPACE" "${SCAN_ARGS[@]}" --json \
    > "$GO_STATE/legacy/migration-plan.json"

  say "copying legacy sources, switching exact links, and verifying every project"
  EWASD_HOME="$GO_STATE" "$GO_BINARY" migrate-legacy \
    --workspace "$LEGACY_WORKSPACE" "${SCAN_ARGS[@]}" --apply --json \
    > "$GO_STATE/legacy/migration-result.json"
else
  say "no Python editors.toml found; skipping legacy link import"
fi

say "verifying the migrated Go state"
EWASD_HOME="$GO_STATE" "$GO_BINARY" status --json > "$TEMP_ROOT/status.json"
if [ -d "$GO_STATE/transactions" ] && [ -n "$(ls -A "$GO_STATE/transactions")" ]; then
  printf 'error: unresolved migration transaction journals remain in %s\n' "$GO_STATE/transactions" >&2
  exit 1
fi

mkdir -p -- "$GO_STATE/legacy"
cat > "$GO_STATE/legacy/replacement-receipt.txt" <<EOF
completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
installed_binary=$INSTALLED
installed_version=$INSTALLED_VERSION
legacy_workspace=$LEGACY_WORKSPACE
go_state=$GO_STATE
source_checkout=$ROOT
EOF
chmod 0600 "$GO_STATE/legacy/replacement-receipt.txt"

say "replacement complete: $INSTALLED_VERSION"
say "legacy workspace retained as rollback source: $LEGACY_WORKSPACE"
say "active Go state: $GO_STATE"
