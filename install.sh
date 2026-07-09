#!/usr/bin/env bash
#
# install.sh — install kiro-anthropic as a systemd service on Linux.
#
# Downloads the latest GitHub release for this machine's architecture, verifies
# its SHA-256 against checksums.txt, installs the binary to /usr/local/bin,
# creates a dedicated system user, and writes + starts a systemd unit.
#
# By default the service binds the API to 127.0.0.1 (loopback only), matching
# the project's security model: reach the API and the admin page from your
# workstation over an SSH tunnel, e.g.
#
#   ssh -L 17890:localhost:17890 -L 27890:localhost:27890 <user>@<server>
#
# Run "sudo ./install.sh --help" for options, and "--uninstall" to remove
# everything this script creates (the exact paths are listed under FILES).
#
# ---------------------------------------------------------------------------
# FILES this script creates/uses (for auditing and manual cleanup):
#   /usr/local/bin/kiro-anthropic            the binary
#   /etc/systemd/system/kiro-anthropic.service  the systemd unit
#   system user + group "kiro" (home /var/lib/kiro-anthropic, no login shell)
#   /var/lib/kiro-anthropic/                 service HOME
#   /var/lib/kiro-anthropic/.kiro-anthropic/accounts.json  the account pool
# ---------------------------------------------------------------------------
set -euo pipefail

REPO="YorrickBao/kiro-anthropic"
APP="kiro-anthropic"
BIN_PATH="/usr/local/bin/${APP}"
UNIT_PATH="/etc/systemd/system/${APP}.service"
SVC_USER="kiro"
SVC_HOME="/var/lib/${APP}"

# Defaults (override via flags/env before install).
HOST="127.0.0.1"
PORT="17890"
ADMIN_PORT="27890"
PROXY="none"
API_KEY=""
VERSION="latest"

usage() {
  cat <<EOF
${APP} installer (Linux + systemd)

USAGE:
  sudo ./install.sh [options]
  sudo ./install.sh --uninstall

OPTIONS:
  --host HOST          API bind address (default: ${HOST}; loopback only).
                       A non-loopback host (e.g. 0.0.0.0) REQUIRES --api-key.
  --port PORT          API port (default: ${PORT}).
  --admin-port PORT    Admin port, always loopback-only (default: ${ADMIN_PORT}).
  --proxy URL          Outbound proxy, or 'none' for direct (default: ${PROXY}).
  --api-key KEY        Require this key from clients (x-api-key / Bearer).
  --version vX.Y.Z     Install a specific release tag (default: latest).
  --uninstall          Stop the service and remove the unit + binary.
                       (Leaves ${SVC_HOME} so credentials are not lost; the
                       command to remove it is printed at the end.)
  -h, --help           Show this help.

SECURITY:
  The admin page has no authentication and is always bound to 127.0.0.1.
  Manage accounts from your workstation over an SSH tunnel:
    ssh -L ${ADMIN_PORT}:localhost:${ADMIN_PORT} <user>@<server>
  then open http://localhost:${ADMIN_PORT} locally.
EOF
}

log()  { printf '==> %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }
die()  { printf '!!  %s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "please run as root (sudo)."
}

detect_arch() {
  local m
  m="$(uname -m)"
  case "$m" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    i386|i686) echo "386" ;;
    *) die "unsupported architecture: $m" ;;
  esac
}

# resolve_tag echoes the release tag to install (latest -> resolved via API).
resolve_tag() {
  if [ "$VERSION" != "latest" ]; then
    echo "$VERSION"
    return
  fi
  local api="https://api.github.com/repos/${REPO}/releases/latest"
  local auth=()
  [ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  curl -fsSL "${auth[@]}" "$api" \
    | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
}

do_install() {
  require_root
  command -v curl >/dev/null 2>&1 || die "curl is required."
  command -v systemctl >/dev/null 2>&1 || die "systemd (systemctl) is required."

  if [ "$HOST" != "127.0.0.1" ] && [ "$HOST" != "localhost" ] && [ "$HOST" != "::1" ] && [ -z "$API_KEY" ]; then
    die "--host ${HOST} is non-loopback; pass --api-key to avoid exposing the account pool (or keep the default 127.0.0.1 and use an SSH tunnel)."
  fi

  local arch tag asset base tmp
  arch="$(detect_arch)"
  tag="$(resolve_tag)"
  [ -n "$tag" ] || die "could not resolve release tag."
  asset="${APP}_${tag}_linux_${arch}.tar.gz"
  base="https://github.com/${REPO}/releases/download/${tag}"

  log "installing ${APP} ${tag} (linux/${arch})"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  log "downloading ${asset}"
  curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}" || die "download failed: ${base}/${asset}"
  curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" || die "download of checksums.txt failed"

  log "verifying checksum"
  ( cd "$tmp" && grep " ${asset}\$" checksums.txt | sha256sum -c - ) \
    || die "checksum verification failed for ${asset}"

  log "extracting"
  tar -xzf "${tmp}/${asset}" -C "$tmp"
  [ -f "${tmp}/${APP}" ] || die "archive did not contain ${APP}"

  # Stop an existing service before replacing the binary (busy file on some FS).
  if systemctl is-active --quiet "${APP}.service" 2>/dev/null; then
    log "stopping running service"
    systemctl stop "${APP}.service"
  fi

  log "installing binary to ${BIN_PATH}"
  install -m 0755 "${tmp}/${APP}" "$BIN_PATH"

  # Dedicated, unprivileged system user with a real HOME for the account store.
  if ! id -u "$SVC_USER" >/dev/null 2>&1; then
    log "creating system user ${SVC_USER} (home ${SVC_HOME})"
    useradd --system --home-dir "$SVC_HOME" --create-home --shell /usr/sbin/nologin "$SVC_USER" \
      || useradd --system --home-dir "$SVC_HOME" --create-home --shell /sbin/nologin "$SVC_USER"
  fi
  mkdir -p "$SVC_HOME"
  chown -R "$SVC_USER:$SVC_USER" "$SVC_HOME"

  # Build the ExecStart args.
  local args="serve --host ${HOST} --port ${PORT} --admin-port ${ADMIN_PORT} --proxy ${PROXY}"
  [ -n "$API_KEY" ] && args="${args} --api-key ${API_KEY}"

  log "writing ${UNIT_PATH}"
  cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=kiro-anthropic (Anthropic-compatible proxy for Kiro)
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SVC_USER}
Group=${SVC_USER}
Environment=HOME=${SVC_HOME}
ExecStart=${BIN_PATH} ${args}
Restart=on-failure
RestartSec=3
# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${SVC_HOME}

[Install]
WantedBy=multi-user.target
UNIT

  log "enabling and starting service"
  systemctl daemon-reload
  systemctl enable --now "${APP}.service"

  sleep 1
  if systemctl is-active --quiet "${APP}.service"; then
    log "service is running."
  else
    warn "service did not start; check: journalctl -u ${APP}.service -e"
  fi

  cat <<DONE

Installed. Summary:
  binary : ${BIN_PATH}
  unit   : ${UNIT_PATH}
  user   : ${SVC_USER} (home ${SVC_HOME})
  data   : ${SVC_HOME}/.kiro-anthropic/accounts.json
  API    : http://${HOST}:${PORT}
  admin  : http://127.0.0.1:${ADMIN_PORT} (loopback only)

Next steps:
  logs   : journalctl -u ${APP}.service -f
  status : systemctl status ${APP}.service
  sign in: the account pool starts empty. From your workstation:
             ssh -L ${ADMIN_PORT}:localhost:${ADMIN_PORT} <user>@<this-server>
           then open http://localhost:${ADMIN_PORT} locally to sign in / import.

To remove everything later:  sudo ./install.sh --uninstall
DONE
}

do_uninstall() {
  require_root
  log "stopping and disabling service"
  systemctl disable --now "${APP}.service" 2>/dev/null || true
  rm -f "$UNIT_PATH"
  systemctl daemon-reload 2>/dev/null || true
  log "removing binary ${BIN_PATH}"
  rm -f "$BIN_PATH"
  cat <<DONE

Removed:
  ${UNIT_PATH}
  ${BIN_PATH}

Left in place (contains your credentials; remove manually if you are done):
  system user/group : ${SVC_USER}
  data directory    : ${SVC_HOME}

  To purge them too:
    sudo userdel ${SVC_USER}
    sudo rm -rf ${SVC_HOME}
DONE
}

main() {
  local action="install"
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --uninstall) action="uninstall" ;;
      --host) HOST="$2"; shift ;;
      --port) PORT="$2"; shift ;;
      --admin-port) ADMIN_PORT="$2"; shift ;;
      --proxy) PROXY="$2"; shift ;;
      --api-key) API_KEY="$2"; shift ;;
      --version) VERSION="$2"; shift ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
    shift
  done
  case "$action" in
    install) do_install ;;
    uninstall) do_uninstall ;;
  esac
}

main "$@"
