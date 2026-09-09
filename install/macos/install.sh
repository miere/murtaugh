#!/usr/bin/env bash
#
# Murtaugh macOS installer (thin orchestrator).
#
# It does four things and nothing else: install the binary, seed a config
# skeleton, write the LaunchAgent plist WITHOUT starting it, and print what is
# left to do.
#
# It used to do much more — prompt for Slack tokens, ask which agent backend to
# use, collect provider credentials, register MCP clients. That was worse in
# every direction: it kept the operator in a terminal answering questions, the
# hard-coded agent list went stale as backends changed, and one mistyped answer
# meant re-running the whole thing. All of that now happens in Slack, where the
# model list is fetched live and a wrong answer is one click to fix.
#
# So this script asks nothing. It is non-interactive by construction rather than
# by a --yes flag, and it deliberately leaves the daemon stopped: Murtaugh can
# do nothing until real Slack tokens are in .env, and an agent started before
# then only crash-loops behind the instructions the operator is still reading.

set -euo pipefail

REPO_OWNER="miere"
REPO_NAME="murtaugh"
RELEASE_API_URL="${MURTAUGH_RELEASE_API_URL:-https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest}"

# ASSUME_YES is accepted and ignored: the installer no longer asks anything, so
# --yes exists only so published curl|bash instructions keep working.
ASSUME_YES=0
SKIP_CONFIG=0
RECONFIGURE=0
FORCE_INSTALL=0
DRY_RUN=0
LOCAL_BUILD=0
TARGET_VERSION=""
# ROLE is which daemons this machine runs. `gateway` is the default because it
# is what this script has always installed; a published curl|bash line that
# passes nothing must keep getting exactly what it got yesterday.
ROLE="gateway"
# GATEWAY_SEED is the ws:// or wss:// address a runtime node dials. --role both
# fills it in for free (the pair is on one machine); --role runtime cannot guess
# it and does not try — it prints the one command that sets it.
GATEWAY_SEED=""
# LOOPBACK_SEED is the address --role both writes. There is no default port
# constant anywhere in the tree, so whatever this picks becomes the de facto
# default and is written down here rather than in three places.
LOOPBACK_SEED="ws://127.0.0.1:8787"

usage() {
  cat <<'EOF'
Usage: install.sh [--role gateway|runtime|both] [--gateway WS_URL] [--version VERSION]
                  [--force] [--skip-config] [--reconfigure] [--dry-run] [--local-build]

Installs or updates Murtaugh on macOS. The installer asks nothing: it puts the
binaries in place, seeds a config skeleton, writes the LaunchAgent plists
without starting them, and prints what is left. The rest of setup — choosing an
agent, its model and its credentials — happens in Slack.

Options:
  --role ROLE           gateway (default), runtime, or both. A runtime node is
                        an always-on daemon that runs agents for a gateway;
                        scheduled jobs must fire when their owner is not
                        chatting. Installing both writes TWO LaunchAgents with
                        distinct labels and separate configuration roots, so a
                        crash-looping node does not take Slack down with it.
  --gateway WS_URL      Seed address a runtime node dials, ws:// or wss://.
                        --role both defaults it to the loopback pair.
  --yes                 Accepted and ignored; nothing is interactive any more.
  --version VERSION     Install a specific version instead of the latest.
  --force               Reinstall even if the current version matches latest.
  --skip-config         Update binary only; do not touch the config directory.
  --reconfigure         Re-seed the config skeleton and templates, backing up
                        anything replaced. Your .env and stored config are kept.
  --dry-run             Show what would happen without making changes.
  --local-build         Compile from the local checkout instead of fetching a
                        release. Useful for testing changes before cutting a
                        new tag. Requires a Go toolchain on PATH.
  --help, -h            Show this message.

Environment overrides:
  MURTAUGH_INSTALL_DIR
  MURTAUGH_RELEASE_JSON_PATH      local file used instead of GitHub API
  MURTAUGH_INSTALL_ARCH           override uname arch for testing
  MURTAUGH_DRY_RUN                yes|no
  MURTAUGH_FORCE_INSTALL          yes|no
  MURTAUGH_RECONFIGURE            yes|no
  MURTAUGH_SKIP_CONFIG            yes|no
  MURTAUGH_TARGET_VERSION         install specific version
  MURTAUGH_ROLE                   gateway|runtime|both
  MURTAUGH_GATEWAY                seed address for a runtime node
EOF
}

log() { printf '[murtaugh-installer] %s\n' "$*" >&2; }
die() { printf '[murtaugh-installer] ERROR: %s\n' "$*" >&2; exit 1; }
timestamp() { date +%Y%m%d%H%M%S; }

# resolve_path canonicalizes $1 without invoking python or GNU coreutils.
# Symlinks are resolved one hop (sufficient for our install dirs); for files
# the parent is resolved and the basename re-attached.
resolve_path() {
  local target=$1
  [[ -n "$target" ]] || { printf ''; return 0; }
  if [[ -L "$target" ]]; then
    local link
    link=$(readlink "$target")
    [[ "$link" = /* ]] || link="$(dirname "$target")/$link"
    target=$link
  fi
  if [[ -d "$target" ]]; then
    (cd "$target" >/dev/null 2>&1 && pwd -P) || printf '%s' "$target"
  else
    local d f
    d=$(dirname "$target"); f=$(basename "$target")
    if (cd "$d" >/dev/null 2>&1); then
      printf '%s/%s' "$(cd "$d" && pwd -P)" "$f"
    else
      printf '%s' "$target"
    fi
  fi
}

backup_file_if_exists() {
  local file=$1
  if [[ -e "$file" ]]; then
    local backup="${file}.bak.$(timestamp)"
    cp -p "$file" "$backup"
    log "Backed up ${file} to ${backup}"
  fi
}

require_darwin() {
  [[ "$(uname -s)" == "Darwin" ]] || die "this installer currently supports macOS only"
}

is_env_yes() {
  local val=${1:-}
  [[ "${val}" == "yes" || "${val}" == "true" || "${val}" == "1" ]]
}

installed_murtaugh_bin() { command -v murtaugh 2>/dev/null || true; }

detect_installed_version() {
  local bin=${1:-}
  if [[ -z "$bin" || ! -x "$bin" ]]; then printf '%s' ""; return 0; fi
  "$bin" version 2>/dev/null || true
}

strip_leading_v() {
  local v="$1"; v="${v#v}"; v="${v#V}"
  printf '%s' "$v"
}

version_compare() {
  local a b
  a="$(strip_leading_v "$1")"
  b="$(strip_leading_v "$2")"
  local IFS=. a_parts b_parts
  read -r -a a_parts <<< "$a"
  read -r -a b_parts <<< "$b"
  local max=$(( ${#a_parts[@]} > ${#b_parts[@]} ? ${#a_parts[@]} : ${#b_parts[@]} ))
  for (( i = 0; i < max; i++ )); do
    local av=${a_parts[i]:-0}
    local bv=${b_parts[i]:-0}
    if (( av > bv )); then return 1; fi
    if (( av < bv )); then return 2; fi
  done
  return 0
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --yes) ASSUME_YES=1 ;;
      --version)
        [[ -n "${2:-}" && "${2:-}" != -* ]] || die "--version requires a value"
        TARGET_VERSION="$2"; shift ;;
      --force) FORCE_INSTALL=1 ;;
      --skip-config) SKIP_CONFIG=1 ;;
      --reconfigure) RECONFIGURE=1 ;;
      --dry-run) DRY_RUN=1 ;;
      --local-build) LOCAL_BUILD=1 ;;
      --role)
        [[ -n "${2:-}" && "${2:-}" != -* ]] || die "--role requires a value (gateway, runtime or both)"
        ROLE="$2"; shift ;;
      --gateway)
        [[ -n "${2:-}" && "${2:-}" != -* ]] || die "--gateway requires a ws:// or wss:// address"
        GATEWAY_SEED="$2"; shift ;;
      --help|-h) usage; exit 0 ;;
      *) die "unknown argument: $1" ;;
    esac
    shift
  done
  is_env_yes "${MURTAUGH_DRY_RUN:-}" && DRY_RUN=1
  is_env_yes "${MURTAUGH_FORCE_INSTALL:-}" && FORCE_INSTALL=1
  is_env_yes "${MURTAUGH_RECONFIGURE:-}" && RECONFIGURE=1
  is_env_yes "${MURTAUGH_SKIP_CONFIG:-}" && SKIP_CONFIG=1
  is_env_yes "${MURTAUGH_LOCAL_BUILD:-}" && LOCAL_BUILD=1
  [[ -n "${MURTAUGH_TARGET_VERSION:-}" ]] && TARGET_VERSION="$MURTAUGH_TARGET_VERSION"
  [[ -n "${MURTAUGH_ROLE:-}" ]] && ROLE="$MURTAUGH_ROLE"
  [[ -n "${MURTAUGH_GATEWAY:-}" ]] && GATEWAY_SEED="$MURTAUGH_GATEWAY"
  case "$ROLE" in
    gateway|runtime|both) ;;
    *) die "unknown role: ${ROLE} (expected gateway, runtime or both)" ;;
  esac
  # --role both is the loopback pair on one machine, so the seed is knowable and
  # an operator who has to supply it by hand for a localhost install would
  # rightly ask why.
  if [[ "$ROLE" == "both" && -z "$GATEWAY_SEED" ]]; then
    GATEWAY_SEED="$LOOPBACK_SEED"
  fi
  return 0
}

# role_runs_gateway / role_runs_runtime keep every branch below reading as the
# question it is asking, rather than as a string comparison repeated eight times.
role_runs_gateway() { [[ "$ROLE" == "gateway" || "$ROLE" == "both" ]]; }
role_runs_runtime() { [[ "$ROLE" == "runtime" || "$ROLE" == "both" ]]; }

choose_install_dir() {
  if [[ -n "${MURTAUGH_INSTALL_DIR:-}" ]]; then
    mkdir -p "$MURTAUGH_INSTALL_DIR"
    printf '%s' "$(resolve_path "$MURTAUGH_INSTALL_DIR")"
    return 0
  fi
  local candidates=() current dir
  current=$(command -v murtaugh 2>/dev/null || true)
  [[ -n "$current" ]] && candidates+=("$(dirname "$(resolve_path "$current")")")
  candidates+=("$HOME/.local/bin")
  [[ -d /opt/homebrew/bin ]] && candidates+=("/opt/homebrew/bin")
  [[ -d /usr/local/bin ]] && candidates+=("/usr/local/bin")
  for dir in "${candidates[@]}"; do
    [[ -n "$dir" ]] || continue
    if [[ "$dir" == "$HOME"/* ]]; then
      mkdir -p "$dir"; printf '%s' "$(resolve_path "$dir")"; return 0
    fi
    [[ -w "$dir" ]] && { printf '%s' "$(resolve_path "$dir")"; return 0; }
  done
  mkdir -p "$HOME/.local/bin"
  printf '%s' "$(resolve_path "$HOME/.local/bin")"
}

# release_json fetches the GitHub release metadata, or reads a local file
# when MURTAUGH_RELEASE_JSON_PATH is set (used by the integration tests).
#
# GitHub's /releases/latest endpoint deliberately excludes pre-releases, so
# when a project has only pre-release tags published it returns 404. We fall
# back to /releases?per_page=1 in that case, which returns the most recent
# release of any kind. The bash JSON extractors operate on regex matches and
# do not care whether the body is a single object or a one-element array.
release_json() {
  local target_version="${1:-}" body
  if [[ -n "${MURTAUGH_RELEASE_JSON_PATH:-}" ]]; then
    cat "$MURTAUGH_RELEASE_JSON_PATH"
    return $?
  fi
  if [[ -n "$target_version" ]]; then
    curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/tags/${target_version}"
    return $?
  fi
  if body=$(curl -fsSL "$RELEASE_API_URL" 2>/dev/null); then
    printf '%s' "$body"
    return 0
  fi
  log "No stable release found; checking for pre-releases."
  curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases?per_page=1"
}

detect_arch_suffix() {
  local arch=${MURTAUGH_INSTALL_ARCH:-$(uname -m)}
  case "$arch" in
    arm64|aarch64) printf 'darwin-arm64' ;;
    x86_64|amd64) printf 'darwin-amd64' ;;
    *) die "unsupported macOS architecture: $arch" ;;
  esac
}

# extract_tag_name pulls "tag_name": "<v>" from GitHub release JSON.
# Pure bash + grep/sed so the installer has no Python dependency.
extract_tag_name() {
  printf '%s' "$1" \
    | tr -d '\n' \
    | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' \
    | head -n1 \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/'
}

# extract_asset_url finds the browser_download_url whose path ends with the
# expected asset filename. Works because release URLs are predictable:
# https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>.
extract_asset_url() {
  local json=$1 want=$2
  printf '%s' "$json" \
    | tr -d '\n' \
    | grep -oE "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[a-z]+://[^\"]*/${want}\"" \
    | head -n1 \
    | sed -E 's/.*"([a-z]+:\/\/[^"]+)".*/\1/'
}

install_or_update_binary() {
  local install_dir=$1 suffix=$2 target_version=${3:-}
  local json tag asset_url tmpdir tmpbin dest installed_bin current_version want

  if ! json=$(release_json "$target_version" 2>/dev/null); then
    die "could not fetch release metadata from GitHub. The repository may have no published releases yet, or you are offline. Set MURTAUGH_RELEASE_JSON_PATH to a local release.json to install from a fixture, or pass --version <tag> to target a specific release."
  fi
  [[ -n "$json" ]] || die "release metadata was empty"

  tag=$(extract_tag_name "$json")
  [[ -n "$tag" ]] || die "release metadata did not contain a tag_name; the response may not be a GitHub release payload"
  want="murtaugh-${tag}-${suffix}"
  asset_url=$(extract_asset_url "$json" "$want")
  [[ -n "$asset_url" ]] || die "release ${tag} has no asset named ${want}"

  installed_bin=$(installed_murtaugh_bin)
  current_version=$(detect_installed_version "$installed_bin")

  if [[ -n "$current_version" && -n "$tag" && "$FORCE_INSTALL" -eq 0 ]]; then
    version_compare "$current_version" "$tag"
    local cmp=$?
    if [[ "$cmp" -eq 0 ]]; then
      log "Already running ${tag} — no update needed. Use --force to reinstall."
      printf '%s' "$(resolve_path "$installed_bin")"; return 0
    elif [[ "$cmp" -eq 1 ]]; then
      log "Already running a newer version (${current_version}) than ${tag} — skipping update."
      printf '%s' "$(resolve_path "$installed_bin")"; return 0
    fi
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would download ${tag} to ${install_dir}/murtaugh"
    printf '%s' "${install_dir}/murtaugh"; return 0
  fi

  tmpdir=$(mktemp -d); tmpbin="$tmpdir/murtaugh"
  curl -fsSL "$asset_url" -o "$tmpbin"
  chmod +x "$tmpbin"
  # Sanity-check the download executes at all. We accept any non-127/126
  # exit because not every released binary version has the same subcommand
  # set; earlier builds shipped without `version` and would otherwise be
  # rejected here even though they run fine.
  "$tmpbin" version >/dev/null 2>&1
  local check_rc=$?
  if [[ $check_rc -eq 127 || $check_rc -eq 126 ]]; then
    die "downloaded release asset for ${tag} could not be executed (exit ${check_rc}); the archive may be corrupted or for the wrong architecture"
  fi
  dest="$install_dir/murtaugh"
  backup_file_if_exists "$dest"
  cp "$tmpbin" "$dest"
  chmod 755 "$dest"
  rm -rf "$tmpdir"
  if [[ "$current_version" == "" ]]; then
    log "Installed Murtaugh ${tag} to ${dest}"
  else
    log "Updated Murtaugh from ${current_version} to ${tag}"
  fi
  printf '%s' "$(resolve_path "$dest")"
}

# install_role_binary fetches one of the SPLIT binaries — murtaugh-runtime or
# murtaugh-gateway — for the same release the CLI resolved.
#
# It is separate from install_or_update_binary rather than a parameter on it,
# because that function's whole body is the version comparison it does against
# `murtaugh version`, and neither of these answers that question: they are not on
# PATH, they carry no independent version, and they must simply match whatever
# tag the CLI is at. Running them through the comparison would have skipped the
# download whenever the CLI was already current — which is every reinstall, and
# would leave the runtime LaunchAgent pointing at a binary that was never
# installed. That crash-loops with ENOENT and the only evidence is a log file
# nobody is watching yet.
install_role_binary() {
  local install_dir=$1 suffix=$2 target_version=${3:-} name=$4
  local json tag asset_url tmpdir tmpbin dest want

  if ! json=$(release_json "$target_version" 2>/dev/null); then
    die "could not fetch release metadata for ${name} from GitHub."
  fi
  tag=$(extract_tag_name "$json")
  [[ -n "$tag" ]] || die "release metadata for ${name} did not contain a tag_name"
  want="${name}-${tag}-${suffix}"
  asset_url=$(extract_asset_url "$json" "$want")
  # Named rather than generic: the likely cause is installing a --version that
  # predates the split, and "no asset" without the tag sends the reader nowhere.
  [[ -n "$asset_url" ]] || die "release ${tag} publishes no ${want}. Roles other than --role gateway need a release that ships the split binaries; use --local-build to compile them from a checkout."

  dest="$install_dir/${name}"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would download ${want} to ${dest}"
    printf '%s' "$dest"; return 0
  fi
  tmpdir=$(mktemp -d); tmpbin="$tmpdir/${name}"
  curl -fsSL "$asset_url" -o "$tmpbin"
  chmod +x "$tmpbin"
  backup_file_if_exists "$dest"
  cp "$tmpbin" "$dest"
  chmod 755 "$dest"
  rm -rf "$tmpdir"
  log "Installed ${name} ${tag} to ${dest}"
  printf '%s' "$(resolve_path "$dest")"
}

# find_repo_root locates the repository root when install.sh is invoked
# from a checkout (i.e. via `bash ./install/macos/install.sh`, not via
# `curl | bash`). The check is intentionally strict: we require both
# go.mod and cmd/murtaugh to exist so we never compile from an
# unrelated tree that happens to live two directories up.
find_repo_root() {
  local script_dir root
  # ${BASH_SOURCE[0]:-} guards the `curl | bash` path (empty BASH_SOURCE under
  # set -u): an empty source resolves to the cwd, which fails the go.mod/
  # cmd/murtaugh checks below and correctly reports "not a checkout".
  script_dir=$(cd "$(dirname "${BASH_SOURCE[0]:-}")" && pwd -P)
  root="${script_dir%/install/macos}"
  [[ "$root" != "$script_dir" ]] || { printf ''; return 0; }
  [[ -f "$root/go.mod" && -d "$root/cmd/murtaugh" ]] || { printf ''; return 0; }
  printf '%s' "$root"
}

# build_local_binary compiles ./cmd/murtaugh from the checkout and drops
# the artifact at $install_dir/murtaugh. The embedded version is stamped
# as "dev-<timestamp>" so `setup.update` (which refuses dev builds by
# default) treats it as a developer artifact rather than a real release.
build_local_binary() {
  local install_dir=$1 repo_root=$2
  command -v go >/dev/null 2>&1 || die "--local-build requires a 'go' toolchain on PATH"
  local dest="$install_dir/murtaugh" version
  version="dev-$(timestamp)"
  log "Building Murtaugh from ${repo_root} (version=${version})"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would: go build -o ${dest} ./cmd/murtaugh"
    printf '%s' "$dest"; return 0
  fi
  backup_file_if_exists "$dest"
  ( cd "$repo_root" && go build -ldflags="-X main.version=${version}" -o "$dest" ./cmd/murtaugh ) \
    || die "go build failed; see output above"
  chmod 755 "$dest"
  log "Installed local-build Murtaugh ${version} to ${dest}"
  printf '%s' "$(resolve_path "$dest")"
}

# build_local_role_binary compiles one of the split binaries from the checkout.
# Same version stamp and same destination convention as build_local_binary; kept
# separate because that one is also the fallback path for `murtaugh` itself and
# reports differently.
build_local_role_binary() {
  local install_dir=$1 repo_root=$2 name=$3
  command -v go >/dev/null 2>&1 || die "--local-build requires a 'go' toolchain on PATH"
  local dest="$install_dir/${name}" version
  version="dev-$(timestamp)"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would: go build -o ${dest} ./cmd/${name}"
    printf '%s' "$dest"; return 0
  fi
  backup_file_if_exists "$dest"
  ( cd "$repo_root" && go build -ldflags="-X main.version=${version}" -o "$dest" "./cmd/${name}" ) \
    || die "go build ./cmd/${name} failed; see output above"
  chmod 755 "$dest"
  log "Installed local-build ${name} ${version} to ${dest}"
  printf '%s' "$(resolve_path "$dest")"
}

# binary_supports_setup checks whether the freshly-installed murtaugh
# binary exposes the `setup` command group. Releases predating the
# installer rewrite do not, and calling `setup launchd` against them
# emits `unknown command: setup` mid-install. We use this to fail
# loudly with an actionable message before any state changes.
binary_supports_setup() {
  local bin=$1
  "$bin" setup --help >/dev/null 2>&1
}

# login_home prints the home directory the directory service records for the
# current uid, or nothing when it cannot be resolved.
login_home() {
  dscl . -read "/Users/$(id -un)" NFSHomeDirectory 2>/dev/null \
    | awk '/^NFSHomeDirectory:/ {print $2}'
}

# launchd_domain_is_ours reports whether $HOME actually belongs to the uid whose
# launchd GUI domain we are about to mutate.
#
# Every launchctl call below targets `gui/$(id -u)` — the real login session —
# while the plist path is derived from $HOME. When a caller overrides HOME those
# two disagree, and the "restart" reaches OUT of the sandbox: it boots out the
# live dev.murtaugh, then bootstraps the sandbox's plist (pointing at a binary
# under a temp dir that is deleted moments later) into the real session. The
# daemon then runs from a deleted inode and can never reconnect to Slack.
#
# That is not hypothetical: `go test ./...` runs the installer with HOME set to
# t.TempDir(), which silently replaced a developer's running gateway for ~19
# hours. Refusing to touch launchd unless HOME is the login home is what keeps a
# sandboxed install sandboxed.
launchd_domain_is_ours() {
  local real_home
  real_home=$(login_home)
  [[ -n "$real_home" ]] || return 1
  [[ "$(resolve_path "$real_home")" == "$(resolve_path "$HOME")" ]]
}

# restart_launch_agent_if_needed restarts ONE already-registered agent, named by
# its label. It takes the label rather than assuming dev.murtaugh because there
# are two now, and restarting the gateway when the runtime was updated is both
# wrong and — on a machine serving Slack — noticed.
#
# It still only ever restarts an agent that is ALREADY registered: a fresh
# install leaves both daemons stopped, which is the installer's character and
# not an accident of ordering.
restart_launch_agent_if_needed() {
  local label=$1
  local plist="$HOME/Library/LaunchAgents/${label}.plist" uid
  [[ -f "$plist" ]] || return 0
  command -v launchctl >/dev/null 2>&1 || return 0
  if ! launchd_domain_is_ours; then
    log "Not restarting LaunchAgent ${label} (HOME=${HOME} is not the login home; refusing to touch the live launchd session)"
    return 0
  fi
  uid=$(id -u)
  launchctl print "gui/${uid}/${label}" >/dev/null 2>&1 || return 0
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would restart LaunchAgent ${label}"; return 0
  fi
  log "Restarting LaunchAgent ${label}"
  launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${uid}" "$plist"
  # bootstrap registers the agent but does not reliably honor RunAtLoad, so
  # force the (re)start explicitly — otherwise the daemon sits loaded but
  # never spawns and Slack stays unreachable.
  launchctl kickstart -k "gui/${uid}/${label}"
  log "Restarted LaunchAgent ${label}"
}

# write_launch_agent installs the LaunchAgent plist WITHOUT loading it.
#
# Deliberately not started. Murtaugh cannot do anything until real Slack tokens
# are in .env, and an agent started here only crash-loops in the background
# while the operator is still reading the instructions. Starting it is the last
# step of the printed hand-off, and it is theirs to take.
# install_gateway_launch_agent and install_runtime_launch_agent are the two
# PAIRINGS, and they are functions rather than four lines in main because the
# pairing is the part that goes wrong and main is the part no test can reach.
#
# Each of the three values is a one-token edit away from its neighbour: the
# gateway's plist runs $installed_bin and the node's runs $runtime_bin (which sit
# adjacent on the same line and are both valid paths); the labels differ by a
# suffix. Getting the binary wrong makes the node's plist exec the CLI with no
# arguments, which prints usage, exits, and is respawned forever by KeepAlive
# into runtime.err.log. Getting the label wrong bounces the LIVE Slack gateway on
# every node-only update. Both leave a suite that reads only the far ends — a
# unit test that supplies the path it then asserts on, and a restart helper that
# never reaches launchctl under a temp HOME — entirely green.
#
# Split out, the pairing is a value a test can read: stub the two helpers and
# call these.
install_gateway_launch_agent() {
  local cli=$1 gateway_yaml=$2
  write_launch_agent "$cli" gateway "$cli" "$gateway_yaml"
  restart_launch_agent_if_needed dev.murtaugh
}

install_runtime_launch_agent() {
  local cli=$1 runtime_bin=$2 node_yaml=$3
  write_launch_agent "$cli" runtime "$runtime_bin" "$node_yaml"
  restart_launch_agent_if_needed dev.murtaugh.runtime
}

write_launch_agent() {
  local cli=$1 role=$2 target_bin=$3 config_yaml=$4
  # --config matters here even though setup.launchd writes only into
  # ~/Library/LaunchAgents. Every murtaugh invocation seeds the configuration
  # root it is pointed at BEFORE the tool runs, so a bare call on a node-only
  # machine would create a gateway config.yaml advertising ${SLACK_APP_TOKEN} in
  # ~/.config/murtaugh — the file item 12 removed from a node, re-created by the
  # step that writes its plist. `--role runtime` is what makes that seeding the
  # node's.
  "$cli" --config "$config_yaml" setup launchd --role "$role" --binary-path "$target_bin" >&2
}

# seed_node_root prepares a runtime node's configuration directory.
#
# Its own directory, never a second file in the gateway's: internal/config/migrate
# backs up and restores every top-level file in the directory it runs in, so two
# roles sharing one would let a failed migration in either restore over the
# other's credentials. It also means the node gets its own config.db, .env and
# node-token for free.
#
# Seeded with `--role runtime`, which is the only way to get a config.yaml with no
# `oauth:` block. Seeding it with a plain `setup bootstrap` would put a file
# advertising ${SLACK_APP_TOKEN} on this laptop — an invitation to fill in
# credentials the node must never hold.
seed_node_root() {
  local cli=$1 node_dir=$2 node_yaml="$2/config.yaml"
  mkdir -p "$node_dir"
  chmod 700 "$node_dir" 2>/dev/null || true
  if [[ -f "$node_yaml" && "$RECONFIGURE" -eq 0 ]]; then
    log "Existing node config detected at ${node_yaml}; leaving it alone."
    return 0
  fi
  local boot_args=(--config "$node_yaml" setup bootstrap --role runtime)
  [[ "$RECONFIGURE" -eq 1 ]] && boot_args+=(--force true)
  "$cli" "${boot_args[@]}" >&2
}

# existing_node_seeds lists the gateway addresses this node already holds, one
# per line, in the order it dials them. A node that has none, or a root that does
# not exist yet, prints nothing.
existing_node_seeds() {
  local cli=$1 node_yaml=$2
  [[ -f "$node_yaml" ]] || return 0
  # `cfg node show` prints the node block as JSON, and `gateway` is the only
  # key in it that holds URLs — see internal/config/node.go, where NodeConfig
  # has exactly one field. So every ws:// or wss:// string in that output is a
  # seed, which is what lets this read the list without a JSON parser on a
  # machine that may not have one.
  "$cli" --config "$node_yaml" cfg node show 2>/dev/null |
    grep -oE 'wss?://[^"[:space:],]+' || true
}

# write_node_seed_address records the gateway a node dials, in the node's own
# store rather than in a file the installer writes by hand.
#
# `cfg node set` is one of the few commands that loads at RoleNode, which is what
# makes it runnable here at all: every other subcommand validates a GATEWAY
# config and dies on "oauth.app_token is required" while .env still holds
# placeholders.
#
# It APPENDS, and the appending is done here because the tool does not do it.
# `cfg node set --gateway` assigns the whole list (`cfg.Gateway = v`, and its own
# schema says "repeatable; replaces the list") — which is right for a `set`
# command and wrong for an installer that runs unattended on every update. A node
# is deliberately allowed several seeds, because a gateway is often known by more
# than one name and item 11's failover walks them in order; a re-run passing one
# `--gateway` would otherwise drop the others, silently removing exactly the
# redundancy that exists for the case where one of those names stops resolving.
# So the existing list is read back and re-passed ahead of the new address, which
# also keeps dial ORDER stable: what worked yesterday is still tried first.
write_node_seed_address() {
  local cli=$1 node_yaml=$2 seed=$3
  [[ -n "$seed" ]] || return 0

  local args=() existing
  while IFS= read -r existing; do
    [[ -n "$existing" ]] || continue
    # Already there: nothing to add, and re-passing it once is enough to keep
    # the list identical rather than growing a duplicate on every update.
    [[ "$existing" == "$seed" ]] && seed=""
    args+=(--gateway "$existing")
  done < <(existing_node_seeds "$cli" "$node_yaml")

  [[ -n "$seed" ]] && args+=(--gateway "$seed")
  "$cli" --config "$node_yaml" cfg node set "${args[@]}" >&2
}

main() {
  parse_args "$@"
  require_darwin

  local install_dir arch_suffix installed_bin repo_root runtime_bin=""
  install_dir=$(choose_install_dir)
  arch_suffix=$(detect_arch_suffix)
  if [[ "$LOCAL_BUILD" -eq 1 ]]; then
    repo_root=$(find_repo_root)
    [[ -n "$repo_root" ]] || die "--local-build requires running install.sh from a checkout containing go.mod and cmd/murtaugh"
    installed_bin=$(build_local_binary "$install_dir" "$repo_root")
  else
    installed_bin=$(install_or_update_binary "$install_dir" "$arch_suffix" "$TARGET_VERSION")
  fi

  # The split binaries, for the roles that run them. `murtaugh` is installed for
  # every role because every role needs the CLI — it is what seeds a node's root,
  # points it at a gateway, and mints its token.
  #
  # murtaugh-gateway is installed for --role both and NOT for --role gateway,
  # and that asymmetry is deliberate. The shipping gateway is still
  # `murtaugh slack gateway` under dev.murtaugh; only the broker binary accepts
  # node connections, so `both` is the role that will need it, and downloading it
  # for the DEFAULT role would make every ordinary install depend on an asset
  # older releases do not publish.
  if role_runs_runtime; then
    if [[ "$LOCAL_BUILD" -eq 1 ]]; then
      runtime_bin=$(build_local_role_binary "$install_dir" "$repo_root" "murtaugh-runtime")
    else
      runtime_bin=$(install_role_binary "$install_dir" "$arch_suffix" "$TARGET_VERSION" "murtaugh-runtime")
    fi
  fi
  if [[ "$ROLE" == "both" ]]; then
    if [[ "$LOCAL_BUILD" -eq 1 ]]; then
      build_local_role_binary "$install_dir" "$repo_root" "murtaugh-gateway" >/dev/null
    else
      install_role_binary "$install_dir" "$arch_suffix" "$TARGET_VERSION" "murtaugh-gateway" >/dev/null
    fi
  fi

  if [[ "$SKIP_CONFIG" -eq 1 ]]; then
    log "Done. Binary updated; config untouched."
    [[ "$DRY_RUN" -eq 1 ]] && log "[DRY-RUN] No changes were made."
    log "Murtaugh MCP command: ${installed_bin} mcp"
    return 0
  fi

  # Bail out early with an actionable message when the binary we just put
  # in place does not expose the `setup` command group. Otherwise the
  # next thing the user sees is `unknown command: setup` mid-install.
  if [[ "$DRY_RUN" -eq 0 ]] && ! binary_supports_setup "$installed_bin"; then
    repo_root=$(find_repo_root)
    if [[ -n "$repo_root" ]]; then
      die "the installed Murtaugh (${installed_bin}) does not support 'setup' yet. Re-run with --local-build to compile from ${repo_root}, or use --skip-config to update the binary only."
    fi
    die "the installed Murtaugh (${installed_bin}) does not support 'setup' yet. Upgrade to a release that includes the setup tools, or pass --skip-config to update the binary only."
  fi

  local config_dir gateway_yaml env_file has_config node_dir node_yaml node_token
  config_dir="$HOME/.config/murtaugh"
  gateway_yaml="$config_dir/config.yaml"
  env_file="$config_dir/.env"
  # A SEPARATE ROOT, not a second file in the gateway's directory. See
  # seed_node_root for why the directory is the unit.
  node_dir="$config_dir/node"
  node_yaml="$node_dir/config.yaml"
  node_token="$node_dir/node-token"
  has_config=0
  [[ -f "$gateway_yaml" ]] && has_config=1

  if [[ "$DRY_RUN" -eq 1 ]]; then
    role_runs_gateway && log "[DRY-RUN] Would seed ${gateway_yaml} and ${env_file}, and write the dev.murtaugh plist without loading it."
    role_runs_runtime && log "[DRY-RUN] Would seed ${node_yaml} and write the dev.murtaugh.runtime plist without loading it."
    log "Murtaugh MCP command: ${installed_bin} mcp"
    return 0
  fi

  if role_runs_gateway; then
    mkdir -p "$config_dir"
    chmod 700 "$config_dir" 2>/dev/null || true

    if [[ "$has_config" -eq 1 && "$RECONFIGURE" -eq 0 ]]; then
      log "Existing config detected at ${gateway_yaml}; leaving it alone."
      log "Use --reconfigure to re-seed the skeleton and templates."
    else
      # setup bootstrap seeds the config directory: the config.yaml skeleton, the
      # .env template, and the Block Kit templates. It writes no credentials and
      # asks no questions.
      local boot_args=(setup bootstrap)
      [[ "$RECONFIGURE" -eq 1 ]] && boot_args+=(--force true)
      "$installed_bin" "${boot_args[@]}" >&2
    fi

    install_gateway_launch_agent "$installed_bin" "$gateway_yaml"
  fi

  if role_runs_runtime; then
    seed_node_root "$installed_bin" "$node_dir"
    write_node_seed_address "$installed_bin" "$node_yaml" "$GATEWAY_SEED"
    install_runtime_launch_agent "$installed_bin" "$runtime_bin" "$node_yaml"
  fi

  if role_runs_gateway; then
    log ""
    log "Murtaugh is installed but not running yet. Three steps left:"
    log ""
    log "  1. Put your Slack tokens in   ${env_file}"
    log "       SLACK_APP_TOKEN=xapp-..."
    log "       SLACK_BOT_TOKEN=xoxb-..."
    log "  2. Review the storage backend in ${gateway_yaml}"
    log "       SQLite is the default and needs nothing. Firestore and Postgres"
    log "       are commented out there; uncomment one to run several nodes."
    log "  3. Start it:"
    log "       launchctl kickstart -k gui/\$(id -u)/dev.murtaugh"
    log ""
    log "Then send Murtaugh a direct message in Slack. The first person to do so"
    log "becomes its administrator, and it walks you through creating an agent"
    log "from there. The rest of setup happens in Slack, not here."
  fi

  if role_runs_runtime; then
    # A runtime node needs exactly two things to attach: a token and a seed
    # address. Both live in its own configuration root, and both are printed
    # here — that is the whole of #200's "nothing outside Murtaugh is involved".
    log ""
    log "A runtime node is installed at ${node_dir}, stopped. It needs two things:"
    log ""
    if [[ -n "$GATEWAY_SEED" ]]; then
      log "  1. A gateway to dial:  ${GATEWAY_SEED}  (already recorded)"
    else
      # Not guessed, and not prompted for: the installer asks nothing, and an
      # address it invented would be one more thing to find and correct later.
      log "  1. A gateway to dial. Nothing was recorded, so set it:"
      log "       murtaugh --config ${node_yaml} cfg node set --gateway wss://your-gateway:8787"
    fi
    if [[ -f "$node_token" ]]; then
      # Deliberately not re-minted. The token file is written with O_EXCL and
      # refuses to be overwritten, so a mint-on-every-run installer would abort
      # the second install outright — and would leave an orphan credential
      # registered on the gateway, because the file is written before the record
      # is stored.
      log "  2. A node token — one is already at ${node_token}."
    else
      # The installer cannot mint one. `node token mint` needs a Slack user id
      # (nobody is the admin yet — the first Slack DM claims that) and runs
      # against a VALIDATED gateway config, which does not exist while .env still
      # holds placeholders. So it prints the command instead of failing mid-script.
      log "  2. A node token, minted ON THE GATEWAY by its admin:"
      log "       murtaugh node token mint --node <name> --user <slack-user-id>"
      log "     then put the printed mrtg_node_… secret at ${node_token} (mode 0600)."
    fi
    log ""
    log "  Start it once both are in place:"
    log "       launchctl kickstart -k gui/\$(id -u)/dev.murtaugh.runtime"
    log ""
    log "  It is an always-on daemon on purpose: scheduled jobs must fire when"
    log "  its owner is not chatting, and a job whose node is asleep does not run"
    log "  and is not replayed."
    if [[ "$ROLE" == "both" ]]; then
      log ""
      log "  Note: dev.murtaugh still runs the in-process gateway, which accepts no"
      log "  node connections. murtaugh-gateway is installed alongside it for when"
      log "  this machine switches over; until then the node will dial and retry."
    fi
  fi

  log ""
  log "Murtaugh MCP command: ${installed_bin} mcp"
}

# Only run main when executed directly, so unit tests can source the
# script to exercise individual helpers in isolation.
#
# The `:-$0` default matters for the `curl … | bash` install path: bash then
# reads the script from stdin, BASH_SOURCE is empty, and a bare
# ${BASH_SOURCE[0]} would trip `set -u` ("unbound variable") before main runs.
# Defaulting to $0 makes the comparison true when piped or executed, and still
# false when sourced (BASH_SOURCE[0] is the script path, $0 is the parent shell).
if [[ "${BASH_SOURCE[0]:-$0}" == "${0}" ]]; then
  main "$@"
fi

