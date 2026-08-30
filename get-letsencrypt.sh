#!/usr/bin/env bash
# Obtain a Let's Encrypt certificate for MediaMTX without binding port 80.
#
# Let's Encrypt HTTP-01 always probes public port 80. MediaMTX listens on
# 8554 / 8888 / 8889 / … and this host often cannot use 80, so the default
# is DNS-01 (no HTTP port). TLS-ALPN-01 on 443 is the other option.
#
# Certbot standalone does not support tls-alpn-01. That method uses acme.sh
# or lego instead.
#
# Usage:
#   ./get-letsencrypt.sh -d stream.example.com -e you@example.com
#   ./get-letsencrypt.sh -d stream.example.com -e you@example.com --method tls-alpn
#   CLOUDFLARE_API_TOKEN=... ./get-letsencrypt.sh -d stream.example.com -e you@example.com --method cloudflare
#   ./get-letsencrypt.sh --renew -d stream.example.com
#
# After a successful issue the files server.crt and server.key are written
# next to mediamtx.yml. MediaMTX reloads them automatically (certloader).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DOMAIN=""
EMAIL=""
METHOD="dns"
WILDCARD=0
STAGING=0
RENEW=0
FORCE=0
MTX_DIR="${MTX_DIR:-$SCRIPT_DIR}"
CERT_DIR="${CERT_DIR:-$SCRIPT_DIR/certs/letsencrypt}"
CLOUDFLARE_CREDENTIALS="${CLOUDFLARE_CREDENTIALS:-}"
HTTP_PORT="${HTTP_PORT:-8080}"
ACME_SH_URL="${ACME_SH_URL:-https://raw.githubusercontent.com/acmesh-official/acme.sh/master/acme.sh}"

usage() {
  cat <<'EOF'
Usage: get-letsencrypt.sh -d DOMAIN -e EMAIL [options]

Options:
  -d, --domain DOMAIN       Hostname for the certificate (required)
  -e, --email EMAIL         Let's Encrypt account email (required for first issue)
      --method METHOD       dns | cloudflare | tls-alpn | http-forward (default: dns)
      --wildcard            Also request *.<domain> (DNS methods only)
      --staging             Use Let's Encrypt staging (for tests)
      --renew               Renew existing certificates and recopy to MediaMTX
      --force               Force renewal even if not due
      --mtx-dir DIR         MediaMTX directory (default: script directory)
      --cert-dir DIR        ACME config dir (default: ./certs/letsencrypt)
      --cloudflare-credentials FILE
                            Cloudflare credentials file for --method cloudflare
      --http-port PORT      Local listen port for --method http-forward (default: 8080)
  -h, --help                Show this help

Methods (none of them bind MediaMTX ports):
  dns            DNS-01 via certbot, interactive TXT record. No port 80/443.
  cloudflare     DNS-01 via Cloudflare API. Set CLOUDFLARE_API_TOKEN or
                 --cloudflare-credentials. No port 80/443.
  tls-alpn       TLS-ALPN-01 on public port 443 (must be free).
                 Uses acme.sh or lego; certbot cannot do this challenge.
  http-forward   HTTP-01 on a local port. Public TCP/80 MUST be forwarded
                 to that port (Let's Encrypt always connects to 80).

Environment:
  MTX_DIR, CERT_DIR, CLOUDFLARE_API_TOKEN, CLOUDFLARE_CREDENTIALS, HTTP_PORT
EOF
}

log() { printf '%s\n' "$*"; }
err() { printf 'error: %s\n' "$*" >&2; }
die() { err "$*"; exit 1; }

port_in_use() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -lnt 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]${port}$"
  elif command -v lsof >/dev/null 2>&1; then
    lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
  else
    return 1
  fi
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -d|--domain)
        DOMAIN="${2:-}"; shift 2 ;;
      -e|--email)
        EMAIL="${2:-}"; shift 2 ;;
      --method)
        METHOD="${2:-}"; shift 2 ;;
      --wildcard)
        WILDCARD=1; shift ;;
      --staging)
        STAGING=1; shift ;;
      --renew)
        RENEW=1; shift ;;
      --force)
        FORCE=1; shift ;;
      --mtx-dir)
        MTX_DIR="${2:-}"; shift 2 ;;
      --cert-dir)
        CERT_DIR="${2:-}"; shift 2 ;;
      --cloudflare-credentials)
        CLOUDFLARE_CREDENTIALS="${2:-}"; shift 2 ;;
      --http-port)
        HTTP_PORT="${2:-}"; shift 2 ;;
      -h|--help)
        usage; exit 0 ;;
      *)
        die "unknown argument: $1 (see --help)" ;;
    esac
  done
}

USE_DOCKER=0
CERTBOT=()
DOCKER_IMAGE="certbot/certbot:latest"
TMP_CF_INI=""
CF_CREDS_HOST=""
CF_CREDS_CONTAINER=""
CERT_ARGS=()
ACME_SH=""
LEGO_BIN=""
TLS_CLIENT=""

cleanup() {
  if [[ -n "$TMP_CF_INI" && -f "$TMP_CF_INI" ]]; then
    rm -f "$TMP_CF_INI"
  fi
}
trap cleanup EXIT

has_native_certbot() {
  command -v certbot >/dev/null 2>&1
}

has_docker() {
  command -v docker >/dev/null 2>&1
}

find_acme_sh() {
  if command -v acme.sh >/dev/null 2>&1; then
    command -v acme.sh
    return 0
  fi
  local candidate
  for candidate in \
    "${HOME:-}/.acme.sh/acme.sh" \
    "$CERT_DIR/acme.sh/acme.sh" \
    /root/.acme.sh/acme.sh
  do
    if [[ -x "$candidate" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

install_acme_sh() {
  command -v curl >/dev/null 2>&1 || die "curl is required to download acme.sh for --method tls-alpn"
  mkdir -p "$CERT_DIR/acme.sh"
  log "Downloading acme.sh (certbot cannot do TLS-ALPN-01)..."
  curl -fsSL "$ACME_SH_URL" -o "$CERT_DIR/acme.sh/acme.sh"
  chmod +x "$CERT_DIR/acme.sh/acme.sh"
  ACME_SH="$CERT_DIR/acme.sh/acme.sh"
}

prepare_cloudflare_creds() {
  CF_CREDS_HOST="$CLOUDFLARE_CREDENTIALS"
  if [[ -z "$CF_CREDS_HOST" && -n "${CLOUDFLARE_API_TOKEN:-}" ]]; then
    TMP_CF_INI="$(mktemp "${TMPDIR:-/tmp}/cloudflare.ini.XXXXXX")"
    chmod 600 "$TMP_CF_INI"
    printf 'dns_cloudflare_api_token = %s\n' "$CLOUDFLARE_API_TOKEN" >"$TMP_CF_INI"
    CF_CREDS_HOST="$TMP_CF_INI"
  fi
  [[ -n "$CF_CREDS_HOST" && -f "$CF_CREDS_HOST" ]] || die "Cloudflare credentials missing.
Set CLOUDFLARE_API_TOKEN or pass --cloudflare-credentials FILE
File format:
  dns_cloudflare_api_token = YOUR_TOKEN
Token needs Zone.DNS Edit on the domain zone."
}

build_certbot() {
  mkdir -p "$CERT_DIR" "$CERT_DIR/work" "$CERT_DIR/logs"

  if has_native_certbot && { [[ "$METHOD" != "cloudflare" ]] || certbot plugins 2>/dev/null | grep -q dns-cloudflare; }; then
    USE_DOCKER=0
    CERTBOT=(certbot)
    return
  fi

  if ! has_docker; then
    if [[ "$METHOD" == "cloudflare" ]] && has_native_certbot; then
      die "certbot plugin dns-cloudflare is not installed.
Debian/Ubuntu: sudo apt-get install -y python3-certbot-dns-cloudflare
or install Docker (image certbot/dns-cloudflare)."
    fi
    die "neither certbot nor docker found. Install one of:
  Debian/Ubuntu:  sudo apt-get install -y certbot
  Fedora:         sudo dnf install -y certbot
  macOS:          brew install certbot
  or install Docker and rerun this script"
  fi

  USE_DOCKER=1
  DOCKER_IMAGE="certbot/certbot:latest"
  if [[ "$METHOD" == "cloudflare" ]]; then
    DOCKER_IMAGE="certbot/dns-cloudflare:latest"
  fi

  CERTBOT=(docker run --rm)
  if [[ "$RENEW" -eq 0 && "$METHOD" == "dns" ]]; then
    CERTBOT+=(-it)
  fi
  CERTBOT+=(
    -v "$CERT_DIR:/etc/letsencrypt"
    -v "$CERT_DIR/work:/var/lib/letsencrypt"
    -v "$CERT_DIR/logs:/var/log/letsencrypt"
  )
  if [[ "$METHOD" == "http-forward" && "$RENEW" -eq 0 ]]; then
    CERTBOT+=(-p "${HTTP_PORT}:${HTTP_PORT}")
  fi
  if [[ "$METHOD" == "cloudflare" ]]; then
    prepare_cloudflare_creds
    CERTBOT+=(-v "$CF_CREDS_HOST:/cloudflare.ini:ro")
    CF_CREDS_CONTAINER="/cloudflare.ini"
  fi
  CERTBOT+=("$DOCKER_IMAGE")
}

append_certbot_dirs() {
  if [[ "$USE_DOCKER" -eq 1 ]]; then
    CERT_ARGS+=(
      --config-dir /etc/letsencrypt
      --work-dir /var/lib/letsencrypt
      --logs-dir /var/log/letsencrypt
    )
  else
    CERT_ARGS+=(
      --config-dir "$CERT_DIR"
      --work-dir "$CERT_DIR/work"
      --logs-dir "$CERT_DIR/logs"
    )
  fi
}

copy_pair() {
  local cert="$1"
  local key="$2"
  [[ -f "$cert" && -f "$key" ]] || die "issued files not found:
  cert: $cert
  key:  $key"
  umask 077
  cp -L "$cert" "$MTX_DIR/server.crt"
  cp -L "$key" "$MTX_DIR/server.key"
  chmod 644 "$MTX_DIR/server.crt"
  chmod 600 "$MTX_DIR/server.key"
  log "Wrote $MTX_DIR/server.crt and $MTX_DIR/server.key"
  log "MediaMTX reloads these files automatically; restart is not required."
  log
  log "Point mediamtx.yml at them, for example:"
  cat <<EOF
  hlsEncryption: true
  hlsServerKey: server.key
  hlsServerCert: server.crt
  webrtcEncryption: true
  webrtcServerKey: server.key
  webrtcServerCert: server.crt
  rtspEncryption: optional
  rtspServerKey: server.key
  rtspServerCert: server.crt
EOF
}

find_live_dir() {
  local candidate
  for candidate in \
    "$CERT_DIR/live/${DOMAIN}" \
    "$CERT_DIR/live/${DOMAIN}-0001"
  do
    if [[ -f "$candidate/fullchain.pem" && -f "$candidate/privkey.pem" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done

  local found=""
  found="$(find "$CERT_DIR/live" -mindepth 1 -maxdepth 1 -type d ! -name README 2>/dev/null | sort | tail -n 1 || true)"
  if [[ -n "$found" && -f "$found/fullchain.pem" && -f "$found/privkey.pem" ]]; then
    printf '%s\n' "$found"
    return 0
  fi
  return 1
}

install_certs_certbot() {
  local live
  live="$(find_live_dir)" || die "issued files not found under $CERT_DIR/live"
  copy_pair "$live/fullchain.pem" "$live/privkey.pem"
}

acme_home() {
  printf '%s\n' "$CERT_DIR/acme.sh"
}

install_certs_acme_sh() {
  local home ecc_dir rsa_dir
  home="$(acme_home)"
  ecc_dir="$home/${DOMAIN}_ecc"
  rsa_dir="$home/${DOMAIN}"
  if [[ -f "$ecc_dir/fullchain.cer" && -f "$ecc_dir/${DOMAIN}.key" ]]; then
    copy_pair "$ecc_dir/fullchain.cer" "$ecc_dir/${DOMAIN}.key"
  elif [[ -f "$rsa_dir/fullchain.cer" && -f "$rsa_dir/${DOMAIN}.key" ]]; then
    copy_pair "$rsa_dir/fullchain.cer" "$rsa_dir/${DOMAIN}.key"
  else
    die "acme.sh issued files not found under $home"
  fi
}

install_certs_lego() {
  local base="$CERT_DIR/lego/certificates/${DOMAIN}"
  if [[ -f "${base}.crt" && -f "${base}.key" ]]; then
    copy_pair "${base}.crt" "${base}.key"
  else
    die "lego issued files not found: ${base}.crt / ${base}.key"
  fi
}

resolve_tls_client() {
  if ACME_SH="$(find_acme_sh)"; then
    TLS_CLIENT="acme.sh"
    return
  fi
  if command -v lego >/dev/null 2>&1; then
    LEGO_BIN="$(command -v lego)"
    TLS_CLIENT="lego"
    return
  fi
  if has_docker; then
    TLS_CLIENT="lego-docker"
    return
  fi
  install_acme_sh
  TLS_CLIENT="acme.sh"
}

ensure_port_443_free() {
  if port_in_use 443; then
    die "port 443 is already in use. Stop whatever listens there (nginx, MediaMTX HTTPS, etc.) and retry.
Let's Encrypt TLS-ALPN-01 must reach this host on public TCP/443."
  fi
}

run_tls_alpn_acme_sh() {
  local home args=()
  home="$(acme_home)"
  mkdir -p "$home"
  args=(
    --home "$home"
    --server letsencrypt
    --issue
    --alpn
    -d "$DOMAIN"
  )
  if [[ -n "$EMAIL" ]]; then
    args+=(-m "$EMAIL")
  fi
  [[ "$STAGING" -eq 1 ]] && args+=(--staging)
  [[ "$FORCE" -eq 1 ]] && args+=(--force)
  if [[ "$RENEW" -eq 1 ]]; then
    args=(--home "$home" --server letsencrypt --renew -d "$DOMAIN")
    [[ "$STAGING" -eq 1 ]] && args+=(--staging)
    [[ "$FORCE" -eq 1 ]] && args+=(--force)
  fi
  log "Issuing certificate for ${DOMAIN} via TLS-ALPN-01 (acme.sh, port 443)"
  "$ACME_SH" "${args[@]}"
  install_certs_acme_sh
}

run_tls_alpn_lego() {
  local lego_path="$CERT_DIR/lego"
  local action="run"
  local -a args
  mkdir -p "$lego_path"

  [[ "$RENEW" -eq 1 ]] && action="renew"
  args=(
    --accept-tos
    --email "$EMAIL"
    --domains "$DOMAIN"
    --tls
  )
  [[ "$STAGING" -eq 1 ]] && args+=(--server https://acme-staging-v02.api.letsencrypt.org/directory)
  [[ "$RENEW" -eq 1 && "$FORCE" -eq 1 ]] && args+=(--days 0)

  log "Issuing certificate for ${DOMAIN} via TLS-ALPN-01 (lego, port 443)"
  if [[ "$TLS_CLIENT" == "lego-docker" ]]; then
    docker run --rm --network host \
      -v "$lego_path:/data" \
      goacme/lego:latest \
      --path /data \
      "${args[@]}" \
      "$action"
  else
    "$LEGO_BIN" --path "$lego_path" "${args[@]}" "$action"
  fi
  install_certs_lego
}

run_tls_alpn() {
  [[ -n "$DOMAIN" ]] || die "domain is required (-d)"
  if [[ "$RENEW" -eq 0 && -z "$EMAIL" ]]; then
    die "email is required (-e)"
  fi
  if [[ "$WILDCARD" -eq 1 ]]; then
    die "--wildcard is not possible with TLS-ALPN-01; use --method dns or cloudflare"
  fi
  ensure_port_443_free
  resolve_tls_client
  if [[ "$TLS_CLIENT" != "lego-docker" && "$(id -u)" -ne 0 ]]; then
    log "warning: TLS-ALPN binds port 443; if this fails, rerun as root"
  fi
  case "$TLS_CLIENT" in
    acme.sh) run_tls_alpn_acme_sh ;;
    lego|lego-docker) run_tls_alpn_lego ;;
    *) die "internal error: unknown TLS client $TLS_CLIENT" ;;
  esac
}

run_renew_certbot() {
  local renewal_conf="$CERT_DIR/renewal/${DOMAIN}.conf"
  if [[ -f "$renewal_conf" ]] && grep -q 'authenticator = manual' "$renewal_conf"; then
    die "This certificate was issued with interactive DNS-01 and cannot be auto-renewed.
Re-run without --renew (same -d / -e) and publish a new TXT record.
For automatic renewals use --method cloudflare."
  fi

  build_certbot
  CERT_ARGS=(renew)
  append_certbot_dirs
  [[ "$STAGING" -eq 1 ]] && CERT_ARGS+=(--staging)
  [[ "$FORCE" -eq 1 ]] && CERT_ARGS+=(--force-renewal)

  log "Renewing certificates in $CERT_DIR"
  "${CERTBOT[@]}" "${CERT_ARGS[@]}"
  install_certs_certbot
}

run_issue_certbot() {
  [[ -n "$DOMAIN" ]] || die "domain is required (-d)"
  [[ -n "$EMAIL" ]] || die "email is required (-e)"

  case "$METHOD" in
    dns|cloudflare|http-forward) ;;
    *) die "unknown method: $METHOD" ;;
  esac

  if [[ "$WILDCARD" -eq 1 && "$METHOD" != "dns" && "$METHOD" != "cloudflare" ]]; then
    die "--wildcard requires --method dns or cloudflare"
  fi

  build_certbot

  if [[ "$METHOD" == "http-forward" ]]; then
    log "WARNING: Let's Encrypt still connects to public port 80."
    log "This method only works if 80 is forwarded to local port ${HTTP_PORT}."
    log "If port 80 is blocked, use --method dns or --method tls-alpn instead."
    if port_in_use "$HTTP_PORT"; then
      die "local port ${HTTP_PORT} is already in use"
    fi
  fi

  if [[ "$METHOD" == "cloudflare" && "$USE_DOCKER" -eq 0 ]]; then
    prepare_cloudflare_creds
    CF_CREDS_CONTAINER="$CF_CREDS_HOST"
  fi

  CERT_ARGS=(certonly)
  append_certbot_dirs
  CERT_ARGS+=(
    --agree-tos
    --no-eff-email
    --email "$EMAIL"
    -d "$DOMAIN"
  )
  if [[ "$WILDCARD" -eq 1 ]]; then
    CERT_ARGS+=(-d "*.${DOMAIN}")
  fi
  [[ "$STAGING" -eq 1 ]] && CERT_ARGS+=(--staging)
  [[ "$FORCE" -eq 1 ]] && CERT_ARGS+=(--force-renewal)

  case "$METHOD" in
    dns)
      CERT_ARGS+=(
        --manual
        --preferred-challenges dns
        --manual-public-ip-logging-ok
      )
      log "DNS-01: add the TXT record certbot prints, wait for DNS, then continue."
      log "Record name looks like: _acme-challenge.${DOMAIN}"
      ;;
    cloudflare)
      CERT_ARGS+=(
        --dns-cloudflare
        --dns-cloudflare-credentials "$CF_CREDS_CONTAINER"
        --dns-cloudflare-propagation-seconds 30
        --non-interactive
      )
      ;;
    http-forward)
      CERT_ARGS+=(
        --standalone
        --preferred-challenges http
        --http-01-port "$HTTP_PORT"
        --non-interactive
      )
      ;;
  esac

  log "Issuing certificate for ${DOMAIN} via ${METHOD}"
  "${CERTBOT[@]}" "${CERT_ARGS[@]}"
  install_certs_certbot
}

detect_existing_method() {
  if [[ -d "$CERT_DIR/acme.sh/${DOMAIN}_ecc" || -d "$CERT_DIR/acme.sh/${DOMAIN}" ]]; then
    METHOD="tls-alpn"
    return
  fi
  if [[ -f "$CERT_DIR/lego/certificates/${DOMAIN}.crt" ]]; then
    METHOD="tls-alpn"
    return
  fi
  local renewal_conf="$CERT_DIR/renewal/${DOMAIN}.conf"
  if [[ -f "$renewal_conf" ]] && grep -q 'authenticator = dns-cloudflare' "$renewal_conf"; then
    METHOD="cloudflare"
  elif [[ -f "$renewal_conf" ]] && grep -q 'pref_challs = http-01' "$renewal_conf"; then
    METHOD="http-forward"
  fi
}

main() {
  parse_args "$@"
  MTX_DIR="$(cd "$MTX_DIR" && pwd)"
  mkdir -p "$CERT_DIR"
  CERT_DIR="$(cd "$CERT_DIR" && pwd)"

  if [[ "$RENEW" -eq 1 ]]; then
    if [[ -z "$DOMAIN" ]]; then
      DOMAIN="$(find "$CERT_DIR/live" -mindepth 1 -maxdepth 1 -type d ! -name README 2>/dev/null | head -n 1 | xargs -I{} basename {} || true)"
    fi
    [[ -n "$DOMAIN" ]] || die "no existing certificate found; pass -d DOMAIN"
    if [[ "$METHOD" == "dns" ]]; then
      detect_existing_method
    fi
    if [[ "$METHOD" == "tls-alpn" ]]; then
      run_tls_alpn
    else
      run_renew_certbot
    fi
    return
  fi

  if [[ "$METHOD" == "tls-alpn" ]]; then
    run_tls_alpn
  else
    run_issue_certbot
  fi
}

main "$@"
