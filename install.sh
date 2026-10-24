#!/bin/sh
# Install a verified Linux release into the current user's bin directory.
set -eu

fail() { printf 'bndr: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: sh install.sh [options]
  --version vX.Y.Z   Install a specific release (default: latest stable)
  --bin-dir DIR      Install directory (default: ~/.local/bin)
  --service          Enable/start or restart a user systemd service
  --archive FILE     Install a local release archive with adjacent SHA256SUMS
  --uninstall        Remove binary and service; preserve config and saved settings
  --help            Show this help

Requires Linux, tar, sha256sum, and curl for online installs. No sudo is used.
HELP
}
version=latest
bin_dir=${BNDR_INSTALL_DIR:-"$HOME/.local/bin"}
config_dir=${XDG_CONFIG_HOME:-"$HOME/.config"}
service=no
uninstall=no
archive=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--bin-dir|--archive)
            [ "$#" -ge 2 ] && [ -n "$2" ] || fail "$1 requires a value"
            case "$1" in --version) version=$2;; --bin-dir) bin_dir=$2;; --archive) archive=$2;; esac
            shift 2;;
        --service) service=yes; shift;;
        --uninstall) uninstall=yes; shift;;
        --help|-h) usage; exit 0;;
        *) fail "unknown option: $1";;
    esac
done
[ "$(uname -s)" = Linux ] || fail 'only Linux is supported'
case "$bin_dir" in /*) ;; *) fail '--bin-dir must be an absolute path';; esac
case "$config_dir" in /*) ;; *) fail 'XDG_CONFIG_HOME must be an absolute path';; esac
if printf '%s%s' "$bin_dir" "$config_dir" | LC_ALL=C grep -q '[[:cntrl:]]'; then fail 'paths must not contain control characters'; fi
unit_dir=$config_dir/systemd/user
unit=$unit_dir/bndr.service
env_file=$config_dir/bndr/bndr.env
binary=$bin_dir/bndr

if [ "$uninstall" = yes ]; then
    if [ -f "$unit" ]; then
        command -v systemctl >/dev/null 2>&1 || fail 'systemctl is needed to stop the installed service'
        systemctl --user disable --now bndr.service || fail 'could not stop the service; binary and settings were preserved'
        rm -f -- "$unit"
        systemctl --user daemon-reload
    fi
    rm -f -- "$binary"
    printf 'Removed bndr. Configuration and saved app settings were preserved.\n'
    exit 0
fi
if [ "$service" = yes ]; then
    command -v systemctl >/dev/null 2>&1 || fail 'systemctl is required for --service'
    systemctl --user show-environment >/dev/null 2>&1 || fail 'no user systemd session available; omit --service to install just the binary'
fi
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) fail 'supported architectures: x86_64 and arm64';; esac
asset=bndr_linux_$arch.tar.gz
for tool in tar sha256sum awk mktemp; do command -v "$tool" >/dev/null 2>&1 || fail "missing required command: $tool"; done
work=$(mktemp -d)
staging=
cleanup() { rm -rf -- "$work"; if [ -n "$staging" ]; then rm -f -- "$staging"; fi; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ -n "$archive" ]; then
    [ "$(basename "$archive")" = "$asset" ] || fail "expected archive named $asset"
    [ -f "$archive" ] || fail "archive does not exist: $archive"
    cp -- "$archive" "$work/$asset"
    cp -- "$(dirname "$archive")/SHA256SUMS" "$work/SHA256SUMS" || fail 'SHA256SUMS must be next to the local archive'
else
    command -v curl >/dev/null 2>&1 || fail 'curl is required for online installation'
    base=https://github.com/tonyrowles/bndr/releases
    if [ "$version" = latest ]; then
        resolved=$(curl --proto '=https' --proto-redir '=https' -fsSL --connect-timeout 15 --max-time 60 -o /dev/null -w '%{url_effective}' "$base/latest") || fail 'could not find a release; check that a release has been published'
        case "$resolved" in "$base/tag/"*) version=${resolved##*/};; *) fail 'unexpected release redirect';; esac
    fi
    printf '%s\n' "$version" | LC_ALL=C grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$' || fail 'version must look like v1.2.3 or v1.2.3-rc.1'
    for file in "$asset" SHA256SUMS; do
        curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 --connect-timeout 15 --max-time 300 "$base/download/$version/$file" -o "$work/$file" || fail "download failed: $file ($version)"
    done
fi
expected=$(awk -v file="$asset" '$2 == file { print $1 }' "$work/SHA256SUMS")
printf '%s\n' "$expected" | LC_ALL=C grep -Eq '^[a-fA-F0-9]{64}$' || fail "missing or invalid checksum for $asset"
actual=$(sha256sum "$work/$asset" | awk '{print $1}')
[ "$actual" = "$expected" ] || fail 'checksum mismatch; existing installation was not changed'
tar -xzf "$work/$asset" -C "$work" bndr || fail 'invalid release archive'
[ -f "$work/bndr" ] && [ ! -L "$work/bndr" ] || fail 'archive must contain a regular bndr binary'
chmod 755 "$work/bndr"
installed_version=$("$work/bndr" --version) || fail 'release binary cannot run on this machine'
case "$installed_version" in 'bndr '*) ;; *) fail 'unexpected release binary';; esac
if [ -z "$archive" ] && [ "$installed_version" != "bndr $version" ]; then fail 'binary version does not match the requested release'; fi
mkdir -p -- "$bin_dir"
staging=$(mktemp "$bin_dir/.bndr-install.XXXXXX")
cp -- "$work/bndr" "$staging"
chmod 755 "$staging"
mv -f -- "$staging" "$binary"
staging=
printf 'Installed %s at %s\n' "$installed_version" "$binary"

if [ "$service" = yes ]; then
    mkdir -p -- "$unit_dir" "$(dirname "$env_file")"
    if [ ! -e "$env_file" ]; then
        (umask 077; cat > "$env_file" <<'ENV'
# Optional bndr settings. Uncomment and edit as needed.
# BNDR_HOST=192.168.1.50
# BNDR_PORT=4567
# BNDR_EXCLUDE=8080,9090
# BNDR_CHROMIUM=/usr/bin/chromium
# BNDR_PREVIEWS=0
ENV
        )
    fi
    # Quote paths for systemd; the colon prefix disables environment substitution.
    unit_binary=$(printf '%s' "$binary" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g')
    unit_env=$(printf '%s' "$env_file" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g')
    cat > "$unit" <<UNIT
[Unit]
Description=bndr LAN dashboard for local development apps
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=":$unit_binary"
EnvironmentFile="-$unit_env"
Restart=on-failure
RestartSec=5
TimeoutStopSec=20

[Install]
WantedBy=default.target
UNIT
    systemctl --user daemon-reload
    systemctl --user enable bndr.service
    systemctl --user restart bndr.service
    printf 'User service started. Settings: %s\nLogs: journalctl --user -u bndr -f\n' "$env_file"
else
    printf 'Start with: "%s"\nFor a background service, rerun this installer with --service.\n' "$binary"
fi
case ":$PATH:" in *":$bin_dir:"*) ;; *) printf 'Add %s to your PATH to run bndr by name.\n' "$bin_dir";; esac
printf 'Thumbnails require Chromium or Chrome. Saved app names and hidden settings are preserved.\n'
