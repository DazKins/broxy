#!/usr/bin/env sh
set -eu

REPO="${BROXY_INSTALL_REPO:-DazKins/broxy}"
INSTALL_BIN_DIR="${BROXY_INSTALL_BIN_DIR:-/usr/local/bin}"
VERSION="${BROXY_VERSION:-}"

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *)
      echo "unsupported operating system: $(uname -s)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)
      echo "unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

resolve_version() {
  if [ -n "$VERSION" ]; then
    case "$VERSION" in
      v*) echo "$VERSION" ;;
      *) echo "v$VERSION" ;;
    esac
    return
  fi

  curl -fsSL \
    -H "Accept: application/vnd.github+json" \
    -H "User-Agent: broxy-installer" \
    "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

checksum_tool() {
  if command -v sha256sum >/dev/null 2>&1; then
    echo "sha256sum"
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    echo "shasum -a 256"
    return
  fi
  echo "missing sha256 tool (need sha256sum or shasum)" >&2
  exit 1
}

run_broxy() {
  "$INSTALL_BIN_DIR/broxy" "$@"
}

run_root() {
  if [ "$(id -u)" -ne 0 ]; then
    sudo "$@"
    return
  fi
  "$@"
}

run_broxy_root() {
  run_root "$INSTALL_BIN_DIR/broxy" "$@"
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
RELEASE_TAG="$(resolve_version)"
if [ -z "$RELEASE_TAG" ]; then
  echo "could not resolve broxy version" >&2
  exit 1
fi

ARCHIVE_VERSION="${RELEASE_TAG#v}"
ASSET="broxy_${ARCHIVE_VERSION}_${OS}_${ARCH}.tar.gz"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

ARCHIVE_URL="https://github.com/$REPO/releases/download/$RELEASE_TAG/$ASSET"
CHECKSUMS_URL="https://github.com/$REPO/releases/download/$RELEASE_TAG/checksums.txt"

curl -fsSL "$ARCHIVE_URL" -o "$TMP_DIR/$ASSET"
curl -fsSL "$CHECKSUMS_URL" -o "$TMP_DIR/checksums.txt"

CHECKSUM_LINE="$(grep " $ASSET\$" "$TMP_DIR/checksums.txt" || true)"
if [ -z "$CHECKSUM_LINE" ]; then
  echo "checksum for $ASSET not found" >&2
  exit 1
fi

(
  cd "$TMP_DIR"
  checksum_cmd="$(checksum_tool)"
  if [ "$checksum_cmd" = "sha256sum" ]; then
    printf '%s\n' "$CHECKSUM_LINE" | sha256sum -c -
  else
    expected="$(printf '%s\n' "$CHECKSUM_LINE" | awk '{print $1}')"
    actual="$(shasum -a 256 "$ASSET" | awk '{print $1}')"
    [ "$expected" = "$actual" ]
  fi
)

run_root mkdir -p "$INSTALL_BIN_DIR"
tar -xzf "$TMP_DIR/$ASSET" -C "$TMP_DIR"
run_root install -m 0755 "$TMP_DIR/broxy" "$INSTALL_BIN_DIR/broxy"

CONFIG_INFO="$(run_broxy config path)"
RESOLVED_CONFIG_PATH="$(printf '%s\n' "$CONFIG_INFO" | sed -n 's/^config_path=//p' | head -n 1)"
FIRST_INSTALL=0
if [ ! -f "$RESOLVED_CONFIG_PATH" ]; then
  FIRST_INSTALL=1
  INIT_OUTPUT="$(run_broxy_root init --non-interactive --json)"
  printf '%s\n' "$INIT_OUTPUT"
fi

run_broxy_root service install
if [ "$FIRST_INSTALL" -eq 1 ]; then
  run_broxy_root service start
else
  run_broxy_root service restart
fi

printf 'broxy %s installed at %s/broxy\n' "$RELEASE_TAG" "$INSTALL_BIN_DIR"
printf 'Config path: %s\n' "$RESOLVED_CONFIG_PATH"
