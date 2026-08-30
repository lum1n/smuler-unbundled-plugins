#!/usr/bin/env bash
# Build, sign, package, release, and submit first-party registry plugins.
#
# Usage:
#   ./scripts/publish-registry-plugins.sh [options] [plugin-id ...]
#
# Examples:
#   ./scripts/publish-registry-plugins.sh --check
#   ./scripts/publish-registry-plugins.sh --release --tag plugins-v0.1.0
#   ./scripts/publish-registry-plugins.sh --release --submit
#   ./scripts/publish-registry-plugins.sh github linear
#
# Prerequisites:
#   - smuler CLI (PATH or .build/smuler from `make cli-build`)
#   - Signing key from `smuler plugin keygen`
#   - macOS recommended for universal lipo builds
#   - gh authenticated for --release / --submit

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

DEFAULT_PLUGINS=(
  github
  linear
  jira
  bitbucket
  confluence
  ai-provider
  agent-monitor
  ci-github-actions
  email
  jenkins
  teams
  cursor-cloud-agents
)

SKIP_BUILD=0
CHECK_ONLY=0
DO_RELEASE=0
DO_SUBMIT=0
DRY_RUN=0
UNPUBLISHED_ONLY=0
TAG=""
REPO=""
RELEASE_NOTES="First-party registry plugin packages"
OUT_DIR="$ROOT/.build/registry-plugins"
REGISTRY_REPO="${SMULER_REGISTRY_REPO:-lum1n/smuler-registry}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

log()  { printf '%s\n' "$*"; }
info() { printf "${GREEN}==>${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}warn:${NC} %s\n" "$*" >&2; }
die()  { printf "${RED}error:${NC} %s\n" "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: publish-registry-plugins.sh [options] [plugin-id ...]

Build, sign, and package unbundled first-party plugins for smuler-registry.

Options:
  --skip-build       Reuse existing plugin binaries
  --check            Validate + verify signatures only (no archives)
  --release          Create/upload a GitHub release with all archives
  --submit           Open one PR to lum1n/smuler-registry with all entries
  --tag TAG          Release tag (default: plugins-v<common-version> or plugins-<date>)
  --repo OWNER/REPO  GitHub repo for release assets (default: origin remote)
  --notes TEXT       Release notes body
  --out-dir DIR      Staging directory for archives (default: .build/registry-plugins)
  --dry-run          Print actions without writing/signing/releasing
  --unpublished-only Only plugins whose local version is newer than smuler-registry
  -h, --help         Show this help

Plugins default to the full registry set. Pass ids to limit the batch.

Examples:
  make registry-plugins-publish
  ./publish-registry-plugins.sh --check
  ./publish-registry-plugins.sh --unpublished-only --release --submit
  ./publish-registry-plugins.sh --release --tag plugins-v0.1.0
  ./publish-registry-plugins.sh --release --submit github linear
EOF
}

PLUGINS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1; shift ;;
    --check) CHECK_ONLY=1; shift ;;
    --release) DO_RELEASE=1; shift ;;
    --submit) DO_SUBMIT=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --unpublished-only) UNPUBLISHED_ONLY=1; shift ;;
    --tag) TAG="${2:-}"; [[ -n "$TAG" ]] || die "--tag requires a value"; shift 2 ;;
    --repo) REPO="${2:-}"; [[ -n "$REPO" ]] || die "--repo requires a value"; shift 2 ;;
    --notes) RELEASE_NOTES="${2:-}"; [[ -n "$RELEASE_NOTES" ]] || die "--notes requires a value"; shift 2 ;;
    --out-dir) OUT_DIR="${2:-}"; [[ -n "$OUT_DIR" ]] || die "--out-dir requires a value"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    -*) die "unknown option: $1" ;;
    *) PLUGINS+=("$1"); shift ;;
  esac
done
while [[ $# -gt 0 ]]; do PLUGINS+=("$1"); shift; done
if [[ ${#PLUGINS[@]} -eq 0 ]]; then
  PLUGINS=("${DEFAULT_PLUGINS[@]}")
fi

filter_unpublished() {
  [[ "$UNPUBLISHED_ONLY" -eq 1 ]] || return 0
  local helper="$ROOT/scripts/unpublished-plugins.py"
  [[ -f "$helper" ]] || die "missing $helper"
  local filtered=()
  local line
  while IFS= read -r line; do
    [[ -n "$line" ]] && filtered+=("$line")
  done < <(python3 "$helper" --root "$ROOT" "${PLUGINS[@]}")
  if [[ ${#filtered[@]} -eq 0 ]]; then
    info "No unpublished plugin versions vs registry — nothing to do"
    exit 0
  fi
  info "Unpublished vs registry: ${filtered[*]}"
  PLUGINS=("${filtered[@]}")
}

run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf '+ %q' "$1"
    shift
    printf ' %q' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

resolve_smuler() {
  # Prefer the repo-built CLI so `make registry-plugins-publish` (which depends
  # on cli-build) does not silently use a stale smuler from PATH.
  if [[ -x "$ROOT/.build/smuler" ]]; then
    printf '%s\n' "$ROOT/.build/smuler"
    return
  fi
  if command -v smuler >/dev/null 2>&1; then
    command -v smuler
    return
  fi
  return 1
}

resolve_package_plugin() {
  # Field-preserving packager. The stock `smuler plugin sign` rewrite drops
  # authProviders/icon/oauth/lookupActions from manifests.
  if [[ -x "$ROOT/.build/package-plugin" ]]; then
    printf '%s\n' "$ROOT/.build/package-plugin"
    return
  fi
  if [[ -f "$ROOT/tools/package-plugin/main.go" ]]; then
    mkdir -p "$ROOT/.build"
    if go build -C "$ROOT/tools/package-plugin" -o "$ROOT/.build/package-plugin" .; then
      printf '%s\n' "$ROOT/.build/package-plugin"
      return
    fi
  fi
  return 1
}

detect_repo() {
  if [[ -n "$REPO" ]]; then
    printf '%s\n' "$REPO"
    return
  fi
  local url
  url="$(git -C "$ROOT" remote get-url origin 2>/dev/null || true)"
  if [[ "$url" =~ github.com[:/](.+)/(.+)(\.git)?$ ]]; then
    local owner="${BASH_REMATCH[1]}"
    local name="${BASH_REMATCH[2]}"
    name="${name%.git}"
    printf '%s/%s\n' "$owner" "$name"
    return
  fi
  printf 'lum1n/smuler\n'
}

manifest_field() {
  local file="$1" field="$2"
  python3 - "$file" "$field" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
v = m.get(sys.argv[2], "")
if isinstance(v, (dict, list)):
    import json as _json
    print(_json.dumps(v))
else:
    print(v if v is not None else "")
PY
}

rewrite_entry_url() {
  local entry="$1" url="$2"
  python3 - "$entry" "$url" <<'PY'
import json, sys
path, url = sys.argv[1], sys.argv[2]
with open(path) as f:
    data = json.load(f)
data["url"] = url
with open(path, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
print(path)
PY
}

ensure_prereqs() {
  if PACKAGE_PLUGIN="$(resolve_package_plugin)"; then
    info "Using packager: $PACKAGE_PLUGIN"
  elif [[ "$DRY_RUN" -eq 1 ]]; then
    PACKAGE_PLUGIN="package-plugin"
    warn "package-plugin not built — dry-run will print placeholder commands"
  else
    die "package-plugin not available (tools/package-plugin)"
  fi

  # Optional: stock smuler CLI for --check only.
  if smuler="$(resolve_smuler)"; then
    SMULER="$smuler"
    info "Using smuler CLI (optional checks): $SMULER"
  else
    SMULER=""
  fi

  if [[ "$DRY_RUN" -eq 0 ]]; then
    local key="$HOME/.config/smuler/keys/smuler_ed25519"
    [[ -f "$key" ]] || die "signing key missing at $key — run: go run ./tools/package-plugin -keygen"
  fi

  if [[ "$DRY_RUN" -eq 0 && ( "$DO_RELEASE" -eq 1 || "$DO_SUBMIT" -eq 1 ) ]]; then
    command -v gh >/dev/null 2>&1 || die "gh CLI required for --release/--submit"
  fi

  for id in "${PLUGINS[@]}"; do
    [[ -d "$ROOT/$id" ]] || die "plugin directory not found: $id"
    [[ -f "$ROOT/$id/manifest.json" ]] || die "missing manifest: $id/manifest.json"
    [[ -f "$ROOT/$id/Makefile" ]] || die "missing Makefile: $id/Makefile"
  done
}

default_tag() {
  local versions=()
  local v
  for id in "${PLUGINS[@]}"; do
    v="$(manifest_field "$ROOT/$id/manifest.json" version)"
    versions+=("$v")
  done
  local first="${versions[0]}"
  local same=1
  for v in "${versions[@]}"; do
    if [[ "$v" != "$first" ]]; then
      same=0
      break
    fi
  done
  if [[ "$same" -eq 1 && -n "$first" ]]; then
    printf 'plugins-v%s\n' "$first"
  else
    printf 'plugins-%s\n' "$(date -u +%Y%m%d)"
  fi
}

build_plugins() {
  if [[ "$SKIP_BUILD" -eq 1 ]]; then
    info "Skipping build (--skip-build)"
    return
  fi
  for id in "${PLUGINS[@]}"; do
    info "Building $id"
    run make -C "$ROOT/$id" build
    local exe
    exe="$(manifest_field "$ROOT/$id/manifest.json" executable)"
    if     [[ "$DRY_RUN" -eq 0 && ! -x "$ROOT/$id/$exe" ]]; then
      die "expected binary missing after build: ./$id/$exe"
    fi
  done
}

package_plugins() {
  mkdir -p "$OUT_DIR"
  ARCHIVES=()
  ENTRIES=()

  for id in "${PLUGINS[@]}"; do
    local dir="$ROOT/$id"
    local version archive_name archive_src archive_dst entry_src entry_dst url

    version="$(manifest_field "$dir/manifest.json" version)"
    archive_name="${id}-${version}.tar.gz"

    # Package with the field-preserving signer. Do NOT use `smuler plugin sign`:
    # it rewrites manifests through an allowlist and drops authProviders.
    info "Packaging $id@$version (preserving authProviders)"
    run "$PACKAGE_PLUGIN" -dir "$dir" -out "$dir"

    if [[ "$CHECK_ONLY" -eq 1 ]]; then
      info "Checking $id@$version"
      if [[ -n "${SMULER:-}" ]]; then
        run "$SMULER" plugin publish --check "$dir" || warn "smuler check failed for $id (continuing)"
      else
        # Verify authProviders survived packaging.
        if ! python3 - "$dir/$archive_name" "$id" <<'PY'
import json, sys, tarfile
archive, plugin_id = sys.argv[1], sys.argv[2]
with tarfile.open(archive, "r:gz") as tf:
    member = tf.extractfile(f"{plugin_id}/manifest.json")
    manifest = json.load(member)
aps = manifest.get("authProviders") or []
if plugin_id in ("ai-provider", "bitbucket", "jira", "confluence") and not aps:
    raise SystemExit(f"{plugin_id}: packaged manifest has no authProviders")
print(f"{plugin_id}: authProviders={len(aps)}")
PY
        then
          die "authProviders missing from packaged manifest"
        fi
      fi
      continue
    fi

    archive_src="$dir/$archive_name"
    entry_src="$dir/.smuler/registry-entry.json"
    archive_dst="$OUT_DIR/$archive_name"
    entry_dst="$OUT_DIR/${id}-${version}.json"

    if [[ "$DRY_RUN" -eq 1 ]]; then
      ARCHIVES+=("$archive_dst")
      ENTRIES+=("$entry_dst")
      continue
    fi

    [[ -f "$archive_src" ]] || die "archive not created: $archive_src"
    [[ -f "$entry_src" ]] || die "registry entry not created: $entry_src"

    # Guardrail: never ship ai-provider/bitbucket/jira/confluence without authProviders.
    if [[ "$id" == "ai-provider" || "$id" == "bitbucket" || "$id" == "jira" || "$id" == "confluence" ]]; then
      if ! python3 - "$archive_src" "$id" <<'PY'
import json, sys, tarfile
archive, plugin_id = sys.argv[1], sys.argv[2]
with tarfile.open(archive, "r:gz") as tf:
    member = tf.extractfile(f"{plugin_id}/manifest.json")
    manifest = json.load(member)
aps = manifest.get("authProviders") or []
if not aps:
    raise SystemExit("missing authProviders")
kinds = {a.get("authKind") for a in aps}
print(f"{plugin_id}: {len(aps)} authProviders kinds={sorted(kinds)}")
if plugin_id in ("bitbucket", "jira", "confluence") and "browser_import" not in kinds:
    raise SystemExit("missing browser_import (cookie auth) provider")
PY
      then
        die "$id archive missing authProviders"
      fi
    fi

    cp "$archive_src" "$archive_dst"
    cp "$entry_src" "$entry_dst"

    if [[ -n "$TAG" ]]; then
      url="https://github.com/${REPO}/releases/download/${TAG}/${archive_name}"
      rewrite_entry_url "$entry_dst" "$url"
      # Keep plugin-local entry in sync for --submit convenience
      rewrite_entry_url "$entry_src" "$url"
    fi

    ARCHIVES+=("$archive_dst")
    ENTRIES+=("$entry_dst")
    log "  archive: $archive_dst"
    log "  entry:   $entry_dst"
  done
}

create_release() {
  [[ "$DO_RELEASE" -eq 1 ]] || return 0
  [[ "$CHECK_ONLY" -eq 0 ]] || die "--release cannot be combined with --check"

  if [[ ${#ARCHIVES[@]} -eq 0 ]]; then
    die "no archives to release"
  fi

  info "Creating GitHub release $TAG on $REPO"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    run gh release create "$TAG" "${ARCHIVES[@]}" \
      --repo "$REPO" \
      --title "Registry plugins ${TAG#plugins-}" \
      --notes "$RELEASE_NOTES"
  elif gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
    warn "release $TAG already exists — uploading/clobbering assets"
    run gh release upload "$TAG" "${ARCHIVES[@]}" --repo "$REPO" --clobber
  else
    run gh release create "$TAG" "${ARCHIVES[@]}" \
      --repo "$REPO" \
      --title "Registry plugins ${TAG#plugins-}" \
      --notes "$RELEASE_NOTES"
  fi

  # Ensure entry URLs point at the published assets
  local i id version archive_name url
  for i in "${!PLUGINS[@]}"; do
    id="${PLUGINS[$i]}"
    version="$(manifest_field "$ROOT/$id/manifest.json" version)"
    archive_name="${id}-${version}.tar.gz"
    url="https://github.com/${REPO}/releases/download/${TAG}/${archive_name}"
    if [[ "$DRY_RUN" -eq 0 ]]; then
      rewrite_entry_url "$OUT_DIR/${id}-${version}.json" "$url"
      rewrite_entry_url "$ROOT/$id/.smuler/registry-entry.json" "$url"
    else
      log "+ rewrite url -> $url"
    fi
  done
}

submit_registry() {
  [[ "$DO_SUBMIT" -eq 1 ]] || return 0
  [[ "$CHECK_ONLY" -eq 0 ]] || die "--submit cannot be combined with --check"

  if [[ ${#ENTRIES[@]} -eq 0 ]]; then
    die "no registry entries to submit"
  fi

  # Require real URLs (no placeholder)
  if [[ "$DRY_RUN" -eq 0 ]]; then
    for entry in "${ENTRIES[@]}"; do
      local url
      url="$(python3 -c "import json; print(json.load(open('$entry')).get('url',''))")"
      if [[ "$url" == *"YOUR_USER"* || -z "$url" ]]; then
        die "entry $entry still has a placeholder url — run with --release or set urls first"
      fi
    done
  fi

  local branch="submit-first-party-plugins-${TAG:-batch}"
  branch="${branch//\//-}"
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/smuler-registry-submit.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" RETURN

  local submit_token="${SMULER_REGISTRY_TOKEN:-${GH_TOKEN:-}}"
  if [[ "$DRY_RUN" -eq 0 && -z "$submit_token" ]]; then
    die "SMULER_REGISTRY_TOKEN (or GH_TOKEN) required for --submit to ${REGISTRY_REPO}"
  fi

  info "Submitting ${#ENTRIES[@]} entries to ${REGISTRY_REPO} (branch $branch)"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "+ gh repo clone $REGISTRY_REPO $tmp -- --depth=1"
    for entry in "${ENTRIES[@]}"; do
      log "+ copy $entry -> submissions/plugins/$(basename "$entry")"
    done
    log "+ open PR on branch $branch"
    return 0
  fi

  GH_TOKEN="$submit_token" gh repo clone "$REGISTRY_REPO" "$tmp" -- --depth=1
  # gh clone authenticates the clone, not later `git push`. Install the
  # credential helper so push uses SMULER_REGISTRY_TOKEN / GH_TOKEN.
  GH_TOKEN="$submit_token" gh auth setup-git
  git -C "$tmp" config user.email "41898282+github-actions[bot]@users.noreply.github.com"
  git -C "$tmp" config user.name "github-actions[bot]"
  git -C "$tmp" checkout -b "$branch"
  mkdir -p "$tmp/submissions/plugins"

  local entry base
  for entry in "${ENTRIES[@]}"; do
    base="$(basename "$entry")"
    cp "$entry" "$tmp/submissions/plugins/$base"
    git -C "$tmp" add "submissions/plugins/$base"
  done

  git -C "$tmp" commit -m "Submit first-party plugins (${TAG:-batch})"
  GH_TOKEN="$submit_token" git -C "$tmp" push -u origin "HEAD:refs/heads/${branch}"
  (
    cd "$tmp"
    GH_TOKEN="$submit_token" gh pr create \
      --repo "$REGISTRY_REPO" \
      --title "Submit first-party plugins (${TAG:-batch})" \
      --body "$(cat <<EOF
Automated batch submission from \`publish-registry-plugins.sh\`.

Tag/release: \`${TAG:-n/a}\`
Source repo: \`${REPO}\`

Plugins:
$(printf -- '- %s\n' "${PLUGINS[@]}")
EOF
)"
  )
}

# --- main ---
# Filter first so a no-op publish does not require the signing key.
filter_unpublished
ensure_prereqs
REPO="$(detect_repo)"
if [[ -z "$TAG" ]]; then
  TAG="$(default_tag)"
fi

info "Plugins: ${PLUGINS[*]}"
info "Repo: $REPO"
info "Tag:  $TAG"
info "Out:  $OUT_DIR"
[[ "$DRY_RUN" -eq 1 ]] && warn "dry-run mode — no changes will be written"

ARCHIVES=()
ENTRIES=()

build_plugins
package_plugins
create_release
submit_registry

info "Done"
if [[ "$CHECK_ONLY" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
  log "Staged archives/entries under: $OUT_DIR"
  if [[ "$DO_RELEASE" -eq 0 ]]; then
    log "Next: re-run with --release --tag $TAG to upload assets, then --submit"
  elif [[ "$DO_SUBMIT" -eq 0 ]]; then
    log "Next: re-run with --skip-build --submit --tag $TAG to open the registry PR"
  fi
fi
