#!/usr/bin/env nix-shell
#! nix-shell -i bash -p jq
set -euo pipefail

CONFIG_PATH="$(realpath "$1")"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -n "${2:-}" ]; then
  NIXPKGS="$2"
elif [ -f "$SCRIPT_DIR/flake.lock" ] && command -v jq >/dev/null; then
  NIXPKGS=$(jq -r '.nodes.nixpkgs.locked.rev' "$SCRIPT_DIR/flake.lock")
else
  NIXPKGS="nixos-unstable"
fi

group() { if [ -n "${CI:-}" ]; then echo "::group::$1"; else echo "=== $1 ==="; fi; }
endgroup() { if [ -n "${CI:-}" ]; then echo "::endgroup::"; fi; }
warn() { if [ -n "${CI:-}" ]; then echo "::warning::$1"; else echo "WARNING: $1"; fi; }

# --- Safety check ---
if [ -z "${CI:-}" ]; then
  echo "WARNING: This will activate NixOS on top of your running system."
  echo "It will stop most services and replace /etc management."
  echo ""
  read -rp "Are you sure? [y/N] " confirm
  if [[ "$confirm" != [yY] ]]; then
    echo "Aborted."
    exit 1
  fi
fi

# --- Preflight checks ---
[ "$(uname -s)" = "Linux" ] || { echo "Requires Linux."; exit 1; }
[ -d /run/systemd/system ] || { echo "Requires systemd."; exit 1; }
[ ! -f /etc/NIXOS ] || { echo "Already NixOS. Nothing to do."; exit 1; }
command -v nix-build >/dev/null || { echo "Requires Nix. Install it first."; exit 1; }
sudo -n true 2>/dev/null || { echo "Requires passwordless sudo."; exit 1; }

# --- Detect system ---
ROOT_DEV=$(findmnt -n -o SOURCE /)
ROOT_FSTYPE=$(findmnt -n -o FSTYPE /)
NIX_DEV=$(findmnt -n -o SOURCE /nix 2>/dev/null || echo "")
NIX_FSTYPE=$(findmnt -n -o FSTYPE /nix 2>/dev/null || echo "")
DNS_SERVERS=$(resolvectl status 2>/dev/null \
  | grep -oP 'DNS Servers:\s*\K.*' \
  | head -1 \
  || echo "1.1.1.1")
USERNAME="$(whoami)"
USER_HOME="$HOME"
USER_UID="$(id -u)"

echo "Detected root: device=$ROOT_DEV fsType=$ROOT_FSTYPE"
echo "Detected user: $USERNAME (uid=$USER_UID, home=$USER_HOME)"

# --- Audit: identify what keeps us alive ---
group "Auditing process tree"
ANCESTOR_PIDS=()
PID=$$
while [ "$PID" -gt 1 ]; do
  ANCESTOR_PIDS+=("$PID")
  PID=$(awk '{print $4}' /proc/$PID/stat 2>/dev/null || echo 0)
done
echo "Ancestor PIDs: ${ANCESTOR_PIDS[*]}"

PROTECTED_UNITS=()
for P in "${ANCESTOR_PIDS[@]}"; do
  UNIT=$(grep -o '[^/]*\.service' /proc/$P/cgroup 2>/dev/null | head -1 || true)
  if [ -n "$UNIT" ]; then
    PROTECTED_UNITS+=("$UNIT")
    echo "  PID $P -> $UNIT (PROTECTED)"
  else
    echo "  PID $P -> (no unit)"
  fi
done
PROTECTED_UNITS=($(printf '%s\n' "${PROTECTED_UNITS[@]}" | sort -u))
echo "Protected units: ${PROTECTED_UNITS[*]}"

echo ""
echo "All running services:"
systemctl list-units --type=service --state=running --no-pager --no-legend

echo ""
echo "Service dependency graph (multi-user.target):"
systemctl list-dependencies multi-user.target --no-pager

for UNIT in "${PROTECTED_UNITS[@]}"; do
  echo ""
  echo "Dependencies of $UNIT:"
  systemctl list-dependencies "$UNIT" --no-pager
  echo ""
  echo "Unit file for $UNIT:"
  systemctl cat "$UNIT"
done

echo ""
echo "Network targets:"
systemctl list-dependencies network-online.target --no-pager
endgroup

# --- Build ---
group "Building NixOS system closure"
SYSTEM=$(nix-build "$SCRIPT_DIR/system.nix" \
  --argstr configPath "$CONFIG_PATH" \
  --argstr nixpkgsSrc "$NIXPKGS" \
  --argstr rootDevice "$ROOT_DEV" \
  --argstr rootFsType "$ROOT_FSTYPE" \
  --argstr nixDevice "${NIX_DEV:-}" \
  --argstr nixFsType "${NIX_FSTYPE:-}" \
  --argstr dnsServers "$DNS_SERVERS" \
  --argstr username "$USERNAME" \
  --argstr userHome "$USER_HOME" \
  --argstr userUid "$USER_UID" \
  --no-out-link)
echo "Built: $SYSTEM"
echo "Closure size: $(nix path-info -rSh "$SYSTEM" 2>/dev/null | tail -1 || echo "unknown")"
endgroup

# --- Protect ancestor processes, stop everything else ---
group "Preparing system"
# Mark protected units so switch-to-configuration won't stop them
for UNIT in "${PROTECTED_UNITS[@]}"; do
  echo "Shielding $UNIT"
  sudo mkdir -p "/etc/systemd/system/${UNIT}.d"
  printf '[Unit]\nX-StopOnRemoval=false\nX-StopOnReconfiguration=false\n' \
    | sudo tee "/etc/systemd/system/${UNIT}.d/protect.conf" >/dev/null
done
sudo systemctl daemon-reload

# Stop all non-essential services
STOP_LIST=()
while IFS= read -r UNIT; do
  UNIT=$(echo "$UNIT" | awk '{print $1}')
  DOMINATED=false
  for P in "${PROTECTED_UNITS[@]}"; do
    if [ "$UNIT" = "$P" ]; then DOMINATED=true; break; fi
  done
  # Keep systemd-critical services
  case "$UNIT" in
    systemd-*|dbus*|nix-daemon*) DOMINATED=true ;;
  esac
  if ! $DOMINATED; then
    STOP_LIST+=("$UNIT")
  fi
done < <(systemctl list-units --type=service --state=running --no-pager --no-legend)

if [ ${#STOP_LIST[@]} -gt 0 ]; then
  echo "Stopping ${#STOP_LIST[@]} services: ${STOP_LIST[*]}"
  sudo systemctl stop "${STOP_LIST[@]}" || true
fi

# Preserve protected unit files in /run so they survive /etc takeover
for UNIT in "${PROTECTED_UNITS[@]}"; do
  SRC="/etc/systemd/system/${UNIT}"
  if [ -f "$SRC" ]; then
    echo "Preserving $UNIT to /run/systemd/system/"
    sudo cp "$SRC" "/run/systemd/system/${UNIT}"
  fi
  if [ -d "/etc/systemd/system/${UNIT}.d" ]; then
    sudo mkdir -p "/run/systemd/system/${UNIT}.d"
    sudo cp -a "/etc/systemd/system/${UNIT}.d/." "/run/systemd/system/${UNIT}.d/"
  fi
done

# Clear /etc paths that conflict with NixOS symlinks
ETC_CONFLICTS=(
  /etc/systemd/system
  /etc/systemd/user
  /etc/systemd/system-generators
  /etc/dbus-1
  /etc/fonts
  /etc/terminfo
)
sudo mkdir -p /etc.bak
for DIR in "${ETC_CONFLICTS[@]}"; do
  if [ -d "$DIR" ] && [ ! -L "$DIR" ]; then
    echo "Moving $DIR -> /etc.bak/"
    sudo mv "$DIR" "/etc.bak/$(echo "$DIR" | tr '/' '_')"
  fi
done
sudo systemctl daemon-reload
endgroup

# --- Activate ---
group "Activating NixOS"
NIX_ENV="$(which nix-env)"
HOST_LD_PATHS=$(cat /etc/ld.so.conf.d/*.conf 2>/dev/null | grep -v '^#' | tr '\n' ' ' || true)
sudo bash -c "
  set -euo pipefail
  echo 'Setting system profile...'
  $NIX_ENV --profile /nix/var/nix/profiles/system --set $SYSTEM

  echo 'Linking /run/current-system...'
  ln -sfn /nix/var/nix/profiles/system /run/current-system

  echo 'Running activate script...'
  $SYSTEM/activate

  # Rebuild the dynamic linker cache so host binaries keep working
  echo 'Restoring host library paths...'
  ldconfig $HOST_LD_PATHS

  # Remove Ubuntu's systemd units, generators, and tmpfiles before reload
  echo 'Clearing host systemd artifacts...'
  for d in /usr/lib/systemd/system /usr/lib/systemd/user /usr/lib/systemd/system-generators /usr/lib/systemd/user-generators /usr/lib/tmpfiles.d /lib/systemd/system; do
    if [ -d \$d ]; then mv \$d \${d}.bak; fi
  done

  echo 'Reloading systemd units...'
  /run/current-system/sw/bin/systemctl daemon-reload

  echo 'Creating tmpfiles...'
  /run/current-system/sw/bin/systemd-tmpfiles --create --remove

  echo 'Starting multi-user.target...'
  /run/current-system/sw/bin/systemctl start multi-user.target

  echo 'Waiting for services to stabilize...'
  /run/current-system/sw/bin/systemctl is-system-running --wait || true
"
endgroup

group "Post-activation checks"
export PATH="/run/wrappers/bin:/run/current-system/sw/bin:$PATH"

STATUS=$(systemctl is-system-running || true)
echo "System status: $STATUS"
if [ "$STATUS" = "degraded" ]; then
  echo "Failed services:"
  systemctl --failed --no-pager
  exit 1
fi

echo "Verifying sudo..."
echo "Wrappers: $(ls -la /run/wrappers/bin/sudo 2>&1)"
echo "Mount: $(mount | grep wrappers)"
sudo -n true 2>&1 || exit 1
echo "sudo: ok"
endgroup

if [ -n "${GITHUB_PATH:-}" ]; then
  echo "/run/wrappers/bin" >> "$GITHUB_PATH"
  echo "/run/current-system/sw/bin" >> "$GITHUB_PATH"
else
  echo "NixOS activated. Add /run/wrappers/bin and /run/current-system/sw/bin to your PATH."
fi
