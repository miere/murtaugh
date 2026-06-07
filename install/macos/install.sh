#!/usr/bin/env bash

set -euo pipefail

REPO_OWNER="miere"
REPO_NAME="murtaugh-dev-toolkit"
RELEASE_API_URL="${MURTAUGH_RELEASE_API_URL:-https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest}"

ASSUME_YES=0
SKIP_CONFIG=0
RECONFIGURE=0
FORCE_INSTALL=0
DRY_RUN=0
TARGET_VERSION=""
CUSTOM_AGENT_ARGS=()

usage() {
  cat <<'EOF'
Usage: install.sh [--yes] [--version VERSION] [--force] [--skip-config] [--reconfigure] [--dry-run]

Installs or updates the Murtaugh macOS release. When Murtaugh is already
installed, re-running this installer updates the binary and preserves existing
config files by default. Use --reconfigure to force a full config rewrite.

Options:
  --yes                 Skip interactive prompts (use env vars for all values).
  --version VERSION     Install a specific version instead of the latest.
  --force               Reinstall even if the current version matches latest.
  --skip-config         Update binary only; do not write or modify any config.
  --reconfigure         Always rewrite config files, backing up existing ones.
  --dry-run             Show what would happen without making changes.
  --help, -h            Show this message.

Environment overrides:
  MURTAUGH_INSTALL_DIR
  MURTAUGH_SLACK_APP_TOKEN
  MURTAUGH_SLACK_BOT_TOKEN
  MURTAUGH_ADMIN_USER
  MURTAUGH_CHAT_AGENT             skip|opencode|goose|auggie|custom
  MURTAUGH_CUSTOM_AGENT_COMMAND
  MURTAUGH_CUSTOM_AGENT_ARGS      shell-style argument string
  MURTAUGH_ENABLE_LAUNCH_AGENT    yes|no
  MURTAUGH_LOAD_LAUNCH_AGENT      yes|no
  MURTAUGH_MCP_CLIENT             skip|opencode|auggie|goose
  MURTAUGH_RELEASE_JSON_PATH      local file used instead of GitHub API
  MURTAUGH_INSTALL_ARCH           override uname arch for testing
  MURTAUGH_DRY_RUN                yes|no (same as --dry-run flag)
  MURTAUGH_FORCE_INSTALL          yes|no (same as --force flag)
  MURTAUGH_RECONFIGURE            yes|no (same as --reconfigure flag)
  MURTAUGH_SKIP_CONFIG            yes|no (same as --skip-config flag)
  MURTAUGH_TARGET_VERSION         install specific version (same as --version)
EOF
}

log() {
  printf '[murtaugh-installer] %s\n' "$*" >&2
}

die() {
  printf '[murtaugh-installer] ERROR: %s\n' "$*" >&2
  exit 1
}

timestamp() {
  date +%Y%m%d%H%M%S
}

realpath_py() {
  python3 - "$1" <<'PY'
import os, sys
print(os.path.realpath(sys.argv[1]))
PY
}

yaml_quote() {
  python3 - "$1" <<'PY'
import sys
print("'" + sys.argv[1].replace("'", "''") + "'")
PY
}

json_merge_local_mcp() {
  local target=$1
  local installed_bin=$2
  local mode=$3
  python3 - "$target" "$installed_bin" "$mode" <<'PY'
import json
import pathlib
import sys

target = pathlib.Path(sys.argv[1])
binary = sys.argv[2]
mode = sys.argv[3]

data = {}
if target.exists():
    data = json.loads(target.read_text())

if mode == "opencode":
    data.setdefault("$schema", "https://opencode.ai/config.json")
    mcp = data.setdefault("mcp", {})
    mcp["murtaugh"] = {
        "type": "local",
        "command": [binary, "mcp"],
        "enabled": True,
    }
elif mode == "auggie":
    servers = data.setdefault("mcpServers", {})
    servers["murtaugh"] = {
        "command": binary,
        "args": ["mcp"],
    }
else:
    raise SystemExit(f"unsupported mode: {mode}")

target.parent.mkdir(parents=True, exist_ok=True)
target.write_text(json.dumps(data, indent=2) + "\n")
PY
}

split_shell_words() {
  python3 - "$1" <<'PY'
import shlex
import sys
sys.stdout.write("\0".join(shlex.split(sys.argv[1])))
PY
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

installed_murtaugh_bin() {
  command -v murtaugh 2>/dev/null || true
}

detect_installed_version() {
  local bin=${1:-}
  if [[ -z "$bin" || ! -x "$bin" ]]; then
    printf '%s' ""
    return 0
  fi
  "$bin" version 2>/dev/null || true
}

# strip_leading_v normalizes a version tag for comparison.
strip_leading_v() {
  local v="$1"
  v="${v#v}"
  v="${v#V}"
  printf '%s' "$v"
}

# version_compare returns 0 if a == b, 1 if a > b, 2 if a < b.
# Uses simple dot-separated integer comparison.
version_compare() {
  local a="$(strip_leading_v "$1")"
  local b="$(strip_leading_v "$2")"
  local IFS=.
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
      --yes)
        ASSUME_YES=1
        ;;
      --version)
        if [[ -n "${2:-}" && "${2:-}" != -* ]]; then
          TARGET_VERSION="$2"
          shift
        else
          die "--version requires a value"
        fi
        ;;
      --force)
        FORCE_INSTALL=1
        ;;
      --skip-config)
        SKIP_CONFIG=1
        ;;
      --reconfigure)
        RECONFIGURE=1
        ;;
      --dry-run)
        DRY_RUN=1
        ;;
      --help|-h)
        usage
        exit 0
        ;;
      *)
        die "unknown argument: $1"
        ;;
    esac
    shift
  done

  # Allow environment overrides for boolean flags
  is_env_yes "${MURTAUGH_DRY_RUN:-}" && DRY_RUN=1
  is_env_yes "${MURTAUGH_FORCE_INSTALL:-}" && FORCE_INSTALL=1
  is_env_yes "${MURTAUGH_RECONFIGURE:-}" && RECONFIGURE=1
  is_env_yes "${MURTAUGH_SKIP_CONFIG:-}" && SKIP_CONFIG=1
  if [[ -n "${MURTAUGH_TARGET_VERSION:-}" ]]; then
    TARGET_VERSION="$MURTAUGH_TARGET_VERSION"
  fi
}

prompt_required() {
  local env_name=$1
  local prompt=$2
  local secret=${3:-no}
  local value=${!env_name:-}
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return 0
  fi
  if [[ $ASSUME_YES -eq 1 ]]; then
    die "${env_name} is required when running with --yes"
  fi
  if [[ "$secret" == "yes" ]]; then
    read -r -s -p "$prompt: " value
    printf '\n' >&2
  else
    read -r -p "$prompt: " value
  fi
  [[ -n "$value" ]] || die "$prompt is required"
  printf '%s' "$value"
}

prompt_choice() {
  local env_name=$1
  local prompt=$2
  local default_value=$3
  shift 3
  local choices=("$@")
  local value=${!env_name:-}
  if [[ -z "$value" ]]; then
    if [[ $ASSUME_YES -eq 1 ]]; then
      value=$default_value
    else
      read -r -p "$prompt [$default_value]: " value
      value=${value:-$default_value}
    fi
  fi
  for choice in "${choices[@]}"; do
    if [[ "$value" == "$choice" ]]; then
      printf '%s' "$value"
      return 0
    fi
  done
  die "invalid value '${value}' for ${env_name}; expected one of: ${choices[*]}"
}

choose_install_dir() {
  if [[ -n "${MURTAUGH_INSTALL_DIR:-}" ]]; then
    mkdir -p "$MURTAUGH_INSTALL_DIR"
    printf '%s' "$(realpath_py "$MURTAUGH_INSTALL_DIR")"
    return 0
  fi

  local candidates=()
  local current
  current=$(command -v murtaugh 2>/dev/null || true)
  if [[ -n "$current" ]]; then
    candidates+=("$(dirname "$(realpath_py "$current")")")
  fi
  candidates+=("$HOME/.local/bin")
  [[ -d /opt/homebrew/bin ]] && candidates+=("/opt/homebrew/bin")
  [[ -d /usr/local/bin ]] && candidates+=("/usr/local/bin")

  local dir
  for dir in "${candidates[@]}"; do
    [[ -n "$dir" ]] || continue
    if [[ "$dir" == "$HOME"/* ]]; then
      mkdir -p "$dir"
      printf '%s' "$(realpath_py "$dir")"
      return 0
    fi
    if [[ -w "$dir" ]]; then
      printf '%s' "$(realpath_py "$dir")"
      return 0
    fi
  done

  mkdir -p "$HOME/.local/bin"
  printf '%s' "$(realpath_py "$HOME/.local/bin")"
}

release_json() {
  local target_version="${1:-}"
  if [[ -n "${MURTAUGH_RELEASE_JSON_PATH:-}" ]]; then
    cat "$MURTAUGH_RELEASE_JSON_PATH"
  elif [[ -n "$target_version" ]]; then
    curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/tags/${target_version}"
  else
    curl -fsSL "$RELEASE_API_URL"
  fi
}

detect_arch_suffix() {
  local arch=${MURTAUGH_INSTALL_ARCH:-$(uname -m)}
  case "$arch" in
    arm64|aarch64) printf 'darwin-arm64' ;;
    x86_64|amd64) printf 'darwin-amd64' ;;
    *) die "unsupported macOS architecture: $arch" ;;
  esac
}

read_release_field() {
  local json=$1
  local suffix=$2
  python3 - "$suffix" "$json" <<'PY'
import json
import sys

suffix = sys.argv[1]
data = json.loads(sys.argv[2])
tag = data["tag_name"]
want_name = f"murtaugh-{tag}-{suffix}"
for asset in data.get("assets", []):
    if asset.get("name") == want_name:
        print(tag)
        print(asset["browser_download_url"])
        sys.exit(0)
raise SystemExit(f"release asset not found: {want_name}")
PY
}

install_or_update_binary() {
  local install_dir=$1
  local suffix=$2
  local target_version=${3:-}
  local json tag asset_url tmpdir tmpbin dest installed_bin current_version

  json=$(release_json "$target_version")
  local parsed=()
  while IFS= read -r line; do
    parsed+=("$line")
  done < <(read_release_field "$json" "$suffix")
  tag=${parsed[0]}
  asset_url=${parsed[1]}

  installed_bin=$(installed_murtaugh_bin)
  current_version=$(detect_installed_version "$installed_bin")

  if [[ -n "$current_version" && -n "$tag" && "$FORCE_INSTALL" -eq 0 ]]; then
    version_compare "$current_version" "$tag"
    local cmp=$?
    if [[ "$cmp" -eq 0 ]]; then
      log "Already running ${tag} — no update needed. Use --force to reinstall."
      printf '%s' "$(realpath_py "$installed_bin")"
      return 0
    elif [[ "$cmp" -eq 1 ]]; then
      log "Already running a newer version (${current_version}) than ${tag} — skipping update."
      printf '%s' "$(realpath_py "$installed_bin")"
      return 0
    fi
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would download ${tag} to ${install_dir}/murtaugh"
    printf '%s' "${install_dir}/murtaugh"
    return 0
  fi

  tmpdir=$(mktemp -d)
  tmpbin="$tmpdir/murtaugh"
  curl -fsSL "$asset_url" -o "$tmpbin"
  chmod +x "$tmpbin"
  "$tmpbin" version >/dev/null 2>&1 || die "downloaded release asset for ${tag} failed a version check"
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
  printf '%s' "$(realpath_py "$dest")"
}

resolve_agent_command() {
  local choice=$1
  case "$choice" in
    skip)
      return 0
      ;;
    opencode|goose|auggie)
      command -v "$choice" >/dev/null 2>&1 || die "${choice} is not installed or not on PATH"
      realpath_py "$(command -v "$choice")"
      ;;
    custom)
      local custom_cmd=${MURTAUGH_CUSTOM_AGENT_COMMAND:-}
      if [[ -z "$custom_cmd" ]]; then
        if [[ $ASSUME_YES -eq 1 ]]; then
          die "MURTAUGH_CUSTOM_AGENT_COMMAND is required for custom chat agent in --yes mode"
        fi
        read -r -p "Custom ACP command path: " custom_cmd
      fi
      [[ -x "$custom_cmd" ]] || die "custom command is not executable: ${custom_cmd}"
      realpath_py "$custom_cmd"
      ;;
    *)
      die "unsupported chat agent choice: ${choice}"
      ;;
  esac
}

collect_custom_args() {
  local arg_string=${MURTAUGH_CUSTOM_AGENT_ARGS:-}
  CUSTOM_AGENT_ARGS=()
  if [[ -z "$arg_string" && $ASSUME_YES -eq 0 ]]; then
    read -r -p "Custom ACP command args (optional): " arg_string
  fi
  if [[ -z "$arg_string" ]]; then
    return 0
  fi
  while IFS= read -r arg; do
    CUSTOM_AGENT_ARGS+=("$arg")
  done < <(python3 - "$arg_string" <<'PY'
import shlex
import sys
for item in shlex.split(sys.argv[1]):
    print(item)
PY
)
}

write_slack_yaml() {
  local path=$1
  local app_token=$2
  local bot_token=$3
  local admin_user=$4
  local chat_choice=$5
  backup_file_if_exists "$path"
  mkdir -p "$(dirname "$path")"
  local q_app q_bot q_admin
  q_app=$(yaml_quote "$app_token")
  q_bot=$(yaml_quote "$bot_token")
  q_admin=$(yaml_quote "$admin_user")
  {
    printf 'oauth:\n'
    printf '  app_token: %s\n' "$q_app"
    printf '  bot_token: %s\n\n' "$q_bot"
    printf 'configuration:\n'
    printf '  admin_user: %s\n' "$q_admin"
    printf '  debug: false\n\n'
    if [[ "$chat_choice" == "skip" ]]; then
      printf 'chat: {}\n\n'
    else
      printf 'chat:\n'
      printf '  default_agent: default\n\n'
    fi
    printf 'commands:\n'
    printf '  - name: /murtaugh\n'
    printf '    description: Entrypoint for Murtaugh commands\n'
  } >"$path"
  chmod 600 "$path"
}

write_agents_yaml() {
  local path=$1
  local chat_choice=$2
  shift 2
  local command_path=${1:-}
  shift || true
  local args=("$@")
  backup_file_if_exists "$path"
  mkdir -p "$(dirname "$path")"
  {
    printf 'acp:\n'
    printf '  enabled: %s\n' "$([[ "$chat_choice" == "skip" ]] && echo false || echo true)"
    printf '  startup_timeout: 10s\n'
    printf '  request_timeout: 10m\n'
    printf '  session_idle_timeout: 30m\n'
    printf '  max_sessions: 100\n'
    printf '  stream_append_interval: 750ms\n'
    printf '  stream_min_chunk_chars: 96\n'
    printf '  stream_final_feedback: false\n\n'
    if [[ "$chat_choice" == "skip" ]]; then
      printf 'agents: {}\n'
    else
      printf 'agents:\n'
      printf '  default:\n'
      printf '    command: %s\n' "$(yaml_quote "$command_path")"
      if [[ ${#args[@]} -eq 0 ]]; then
        printf '    args: []\n'
      else
        printf '    args:\n'
        local arg
        for arg in "${args[@]}"; do
          printf '      - %s\n' "$(yaml_quote "$arg")"
        done
      fi
    fi
  } >"$path"
  chmod 600 "$path"
}

write_launch_agent() {
  local installed_bin=$1
  local enable_choice load_choice plist logs_dir uid
  enable_choice=$(prompt_choice MURTAUGH_ENABLE_LAUNCH_AGENT "Create a launchd LaunchAgent?" yes yes no)
  if [[ "$enable_choice" != "yes" ]]; then
    return 0
  fi
  plist="$HOME/Library/LaunchAgents/dev.murtaugh.plist"
  logs_dir="$HOME/Library/Logs/murtaugh"
  mkdir -p "$(dirname "$plist")" "$logs_dir"
  backup_file_if_exists "$plist"
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>dev.murtaugh</string>
    <key>ProgramArguments</key>
    <array>
      <string>${installed_bin}</string>
      <string>slack</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>WorkingDirectory</key>
    <string>${HOME}</string>
    <key>StandardOutPath</key>
    <string>${logs_dir}/slack.out.log</string>
    <key>StandardErrorPath</key>
    <string>${logs_dir}/slack.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
      <key>PATH</key>
      <string>${HOME}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
  </dict>
</plist>
EOF
  command -v plutil >/dev/null 2>&1 && plutil -lint "$plist" >/dev/null
  log "Wrote LaunchAgent ${plist}"
  load_choice=$(prompt_choice MURTAUGH_LOAD_LAUNCH_AGENT "Load the LaunchAgent now?" no yes no)
  if [[ "$load_choice" == "yes" ]]; then
    command -v launchctl >/dev/null 2>&1 || die "launchctl is required to load the LaunchAgent"
    uid=$(id -u)
    launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
    launchctl bootstrap "gui/${uid}" "$plist"
    log "Loaded LaunchAgent dev.murtaugh"
  fi
}

restart_launch_agent_if_needed() {
  local installed_bin=$1
  local plist="$HOME/Library/LaunchAgents/dev.murtaugh.plist"
  if [[ ! -f "$plist" ]]; then
    return 0
  fi
  command -v launchctl >/dev/null 2>&1 || return 0
  local uid
  uid=$(id -u)
  if ! launchctl print "gui/${uid}/dev.murtaugh" >/dev/null 2>&1; then
    return 0
  fi
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would restart LaunchAgent dev.murtaugh"
    return 0
  fi
  log "Restarting LaunchAgent dev.murtaugh"
  launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${uid}" "$plist"
  log "Restarted LaunchAgent dev.murtaugh"
}

configure_mcp_client() {
  local installed_bin=$1
  local mcp_client target
  mcp_client=$(prompt_choice MURTAUGH_MCP_CLIENT "Configure Murtaugh as an MCP server in a client?" skip skip opencode auggie goose)
  case "$mcp_client" in
    skip)
      return 0
      ;;
    opencode)
      command -v opencode >/dev/null 2>&1 || die "OpenCode is not installed or not on PATH"
      target="$HOME/.config/opencode/opencode.json"
      [[ -e "$target" ]] && backup_file_if_exists "$target"
      json_merge_local_mcp "$target" "$installed_bin" opencode || die "failed to update ${target}; if it contains JSONC comments, please edit it manually"
      log "Configured OpenCode MCP in ${target}"
      ;;
    auggie)
      command -v auggie >/dev/null 2>&1 || die "Auggie is not installed or not on PATH"
      target="$HOME/.augment/settings.json"
      [[ -e "$target" ]] && backup_file_if_exists "$target"
      json_merge_local_mcp "$target" "$installed_bin" auggie || die "failed to update ${target}"
      log "Configured Auggie MCP in ${target}"
      ;;
    goose)
      command -v goose >/dev/null 2>&1 || die "Goose is not installed or not on PATH"
      log "Goose MCP setup is manual-only in v1; no files were modified."
      log "Start Goose from your project and add Murtaugh as a stdio extension: goose session --with-extension '${installed_bin} mcp'"
      ;;
  esac
}

main() {
  parse_args "$@"
  require_darwin

  local install_dir arch_suffix installed_bin
  install_dir=$(choose_install_dir)
  arch_suffix=$(detect_arch_suffix)
  installed_bin=$(install_or_update_binary "$install_dir" "$arch_suffix" "$TARGET_VERSION")

  if [[ "$SKIP_CONFIG" -eq 1 ]]; then
    log "Done. Binary updated; config untouched."
    if [[ "$DRY_RUN" -eq 1 ]]; then
      log "[DRY-RUN] No changes were made."
    fi
    log "Murtaugh MCP command: ${installed_bin} mcp"
    return 0
  fi

  local config_dir slack_yaml agents_yaml has_config
  config_dir="$HOME/.config/murtaugh"
  slack_yaml="$config_dir/slack.yaml"
  agents_yaml="$config_dir/agents.yaml"
  has_config=0
  [[ -f "$slack_yaml" || -f "$agents_yaml" ]] && has_config=1

  local app_token bot_token admin_user chat_choice chat_command=""
  local -a chat_args=()

  if [[ "$has_config" -eq 1 && "$RECONFIGURE" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
    log "Existing config detected. Preserving Slack and agent configs by default."
    log "Use --reconfigure to rewrite them."
  else
    if [[ "$DRY_RUN" -eq 1 && "$has_config" -eq 1 && "$RECONFIGURE" -eq 0 ]]; then
      log "[DRY-RUN] Would preserve existing config files."
    elif [[ "$DRY_RUN" -eq 1 && "$RECONFIGURE" -eq 1 ]]; then
      log "[DRY-RUN] Would rewrite config files with backups."
    elif [[ "$DRY_RUN" -eq 1 && "$has_config" -eq 0 ]]; then
      log "[DRY-RUN] Would write new config files."
    fi

    app_token=$(prompt_required MURTAUGH_SLACK_APP_TOKEN "Slack app token (xapp-...)" yes)
    bot_token=$(prompt_required MURTAUGH_SLACK_BOT_TOKEN "Slack bot token (xoxb-...)" yes)
    admin_user=$(prompt_required MURTAUGH_ADMIN_USER "Slack admin handle or user ID")
    [[ "$app_token" == xapp-* ]] || die "Slack app token must start with xapp-"
    [[ "$bot_token" == xoxb-* ]] || die "Slack bot token must start with xoxb-"

    chat_choice=$(prompt_choice MURTAUGH_CHAT_AGENT "Slack Chat agent" skip skip opencode goose auggie custom)
    if [[ "$chat_choice" != "skip" ]]; then
      chat_command=$(resolve_agent_command "$chat_choice")
    fi
    case "$chat_choice" in
      opencode) chat_args=(acp) ;;
      goose) chat_args=(acp) ;;
      auggie) chat_args=(--acp --allow-indexing) ;;
      custom)
        collect_custom_args
        chat_args=("${CUSTOM_AGENT_ARGS[@]}")
        ;;
    esac

    mkdir -p "$config_dir"
    chmod 700 "$config_dir" 2>/dev/null || true

    if [[ "$DRY_RUN" -eq 1 ]]; then
      log "[DRY-RUN] Would write ${slack_yaml} and ${agents_yaml}"
    else
      write_slack_yaml "$slack_yaml" "$app_token" "$bot_token" "$admin_user" "$chat_choice"
      if [[ "$chat_choice" == "skip" ]]; then
        write_agents_yaml "$agents_yaml" "$chat_choice"
      else
        write_agents_yaml "$agents_yaml" "$chat_choice" "$chat_command" "${chat_args[@]}"
      fi
      log "Wrote Slack config to ${slack_yaml}"
      log "Wrote agent config to ${agents_yaml}"
    fi
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would configure LaunchAgent and MCP clients if applicable."
  else
    write_launch_agent "$installed_bin"
    configure_mcp_client "$installed_bin"
  fi

  restart_launch_agent_if_needed "$installed_bin"

  log "Murtaugh MCP command: ${installed_bin} mcp"
  log "Done. Re-run this installer any time to update or regenerate config."
}

main "$@"