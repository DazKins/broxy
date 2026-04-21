#!/usr/bin/env sh
set -eu

INSTALL_BIN_DIR="${BROXY_INSTALL_BIN_DIR:-/usr/local/bin}"

run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    sudo "$@"
    return
  fi
  echo "must run as root or have sudo available" >&2
  return 1
}

try_root() {
  run_root "$@" >/dev/null 2>&1 || true
}

OS="$(uname -s)"

case "$OS" in
  Darwin)
    try_root launchctl bootout system/com.broxy.daemon
    try_root launchctl bootout system /Library/LaunchDaemons/com.broxy.daemon.plist
    try_root rm -f /Library/LaunchDaemons/com.broxy.daemon.plist
    ;;
  Linux)
    try_root systemctl disable --now broxy.service
    try_root rm -f /etc/systemd/system/broxy.service
    if command -v systemctl >/dev/null 2>&1; then
      try_root systemctl daemon-reload
      try_root systemctl reset-failed broxy.service
    fi
    ;;
  *)
    echo "unsupported operating system: $OS" >&2
    exit 1
    ;;
esac

run_root rm -f "$INSTALL_BIN_DIR/broxy"
run_root rm -rf /etc/broxy /var/lib/broxy /var/log/broxy

case "$OS" in
  Darwin)
    try_root dscl . -delete /Users/broxy
    try_root dscl . -delete /Groups/broxy
    ;;
  Linux)
    try_root userdel broxy
    try_root groupdel broxy
    ;;
esac

printf 'broxy has been completely uninstalled\n'
