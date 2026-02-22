#!/usr/bin/env bash

set -euo pipefail

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

repo_owner="clavin-dev"
repo_name="xrayr"
repo_ref="${1:-${XRAYR_REPO_REF:-main}}"
raw_base="https://raw.githubusercontent.com/${repo_owner}/${repo_name}/${repo_ref}"

install_dir="/usr/local/XrayR"
config_dir="/etc/XrayR"
service_file="/etc/systemd/system/XrayR.service"
binary_path="${install_dir}/XrayR"

[[ "${EUID}" -ne 0 ]] && echo -e "${red}Error:${plain} run this script as root." && exit 1

if [[ "$(uname -s)" != "Linux" ]]; then
  echo -e "${red}Error:${plain} this installer currently supports Linux only."
  exit 1
fi

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "linux-amd64" ;;
    aarch64|arm64) echo "linux-arm64" ;;
    *)
      echo -e "${red}Error:${plain} unsupported architecture: $(uname -m)"
      exit 1
      ;;
  esac
}

install_base() {
  if command -v apt >/dev/null 2>&1; then
    apt update -y
    apt install -y curl tar ca-certificates
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y curl tar ca-certificates
  elif command -v yum >/dev/null 2>&1; then
    yum install -y curl tar ca-certificates
  else
    echo -e "${red}Error:${plain} unsupported package manager, please install curl and tar manually."
    exit 1
  fi
}

download() {
  local url="$1"
  local output="$2"
  curl -fL --retry 3 --retry-delay 1 -o "${output}" "${url}"
}

install_binary() {
  local artifact="$1"
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "${tmp_dir}"' EXIT

  mkdir -p "${install_dir}"
  download "${raw_base}/attachments/XrayR-${artifact}.tar.gz" "${tmp_dir}/xrayr.tar.gz"
  tar -xzf "${tmp_dir}/xrayr.tar.gz" -C "${tmp_dir}"

  if [[ ! -f "${tmp_dir}/XrayR-${artifact}" ]]; then
    echo -e "${red}Error:${plain} binary not found inside tarball."
    exit 1
  fi

  install -m 755 "${tmp_dir}/XrayR-${artifact}" "${binary_path}"
}

sync_file_if_absent() {
  local source="$1"
  local target="$2"
  if [[ ! -f "${target}" ]]; then
    download "${source}" "${target}"
  fi
}

sync_configs() {
  mkdir -p "${config_dir}"

  sync_file_if_absent "${raw_base}/release/config/config.yml.example" "${config_dir}/config.yml"
  sync_file_if_absent "${raw_base}/release/config/dns.json" "${config_dir}/dns.json"
  sync_file_if_absent "${raw_base}/release/config/route.json" "${config_dir}/route.json"
  sync_file_if_absent "${raw_base}/release/config/custom_outbound.json" "${config_dir}/custom_outbound.json"
  sync_file_if_absent "${raw_base}/release/config/custom_inbound.json" "${config_dir}/custom_inbound.json"
  sync_file_if_absent "${raw_base}/release/config/rulelist" "${config_dir}/rulelist"
  sync_file_if_absent "${raw_base}/release/config/geosite.dat" "${config_dir}/geosite.dat"
  sync_file_if_absent "${raw_base}/release/config/geoip.dat" "${config_dir}/geoip.dat"
}

setup_service() {
  cat > "${service_file}" <<EOF
[Unit]
Description=XrayR Service
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
LimitAS=infinity
LimitRSS=infinity
LimitCORE=infinity
LimitNOFILE=999999
WorkingDirectory=${install_dir}
ExecStart=${binary_path} --config ${config_dir}/config.yml
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable XrayR >/dev/null 2>&1 || true
  systemctl restart XrayR
}

install_manager_script() {
  cat > /usr/bin/XrayR <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

service="XrayR"
repo_owner="clavin-dev"
repo_name="xrayr"

usage() {
  cat <<USAGE
XrayR                - Show this help
XrayR start          - Start XrayR
XrayR stop           - Stop XrayR
XrayR restart        - Restart XrayR
XrayR status         - Show XrayR status
XrayR enable         - Enable auto-start
XrayR disable        - Disable auto-start
XrayR log            - Show logs
XrayR config         - Edit config
XrayR update [ref]   - Reinstall from repo ref (default: main)
XrayR uninstall      - Uninstall XrayR
XrayR version        - Show XrayR version
USAGE
}

case "${1:-}" in
  start) systemctl start "${service}" ;;
  stop) systemctl stop "${service}" ;;
  restart) systemctl restart "${service}" ;;
  status) systemctl status "${service}" --no-pager -l ;;
  enable) systemctl enable "${service}" ;;
  disable) systemctl disable "${service}" ;;
  log) journalctl -u "${service}" -e --no-pager -f ;;
  config) "${EDITOR:-vi}" /etc/XrayR/config.yml ;;
  update)
    ref="${2:-main}"
    bash <(curl -fsSL "https://raw.githubusercontent.com/${repo_owner}/${repo_name}/${ref}/install.sh") "${ref}"
    ;;
  uninstall)
    systemctl stop "${service}" || true
    systemctl disable "${service}" || true
    rm -f /etc/systemd/system/XrayR.service
    systemctl daemon-reload
    systemctl reset-failed || true
    rm -rf /etc/XrayR
    rm -rf /usr/local/XrayR
    rm -f /usr/bin/XrayR /usr/bin/xrayr
    echo "XrayR uninstalled."
    ;;
  version)
    if [[ -x /usr/local/XrayR/XrayR ]]; then
      /usr/local/XrayR/XrayR version || true
    else
      echo "XrayR binary not found."
      exit 1
    fi
    ;;
  *)
    usage
    ;;
esac
EOF

  chmod +x /usr/bin/XrayR
  ln -sf /usr/bin/XrayR /usr/bin/xrayr
}

main() {
  echo -e "${green}Installing XrayR from:${plain} ${repo_owner}/${repo_name}@${repo_ref}"
  install_base

  artifact="$(detect_arch)"
  echo -e "${yellow}Detected architecture:${plain} ${artifact}"

  install_binary "${artifact}"
  sync_configs
  setup_service
  install_manager_script

  if systemctl is-active --quiet XrayR; then
    echo -e "${green}XrayR installed and running.${plain}"
  else
    echo -e "${red}XrayR installed but not running.${plain} Check logs with: XrayR log"
  fi
}

main "$@"
