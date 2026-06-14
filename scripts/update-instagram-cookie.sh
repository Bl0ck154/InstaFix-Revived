#!/usr/bin/env bash
set -euo pipefail

COOKIE_DIR="${INSTAFIX_COOKIE_DIR:-/opt/instafix-revived/secrets/instagram_cookies}"
LEGACY_COOKIE_FILE="${INSTAFIX_COOKIE_FILE:-/opt/instafix-revived/secrets/instagram_cookie}"
HELPER_CONTAINER="${INSTAFIX_HELPER_CONTAINER:-instafix-auth-helper}"
HELPER_HEALTH_URL="${INSTAFIX_HELPER_HEALTH_URL:-http://127.0.0.1:3200/healthz}"
HELPER_ACCOUNTS_URL="${INSTAFIX_HELPER_ACCOUNTS_URL:-http://127.0.0.1:3200/accounts}"
HELPER_TEST_POST="${INSTAFIX_HELPER_TEST_POST:-DW9rpAdCeCK}"
HELPER_TEST_URL="${INSTAFIX_HELPER_TEST_URL:-http://127.0.0.1:3200/oembed/${HELPER_TEST_POST}}"

umask 077

if [[ $EUID -ne 0 ]]; then
  echo "Run as root: sudo $0" >&2
  exit 1
fi

usage() {
  cat <<EOF
InstaFix Instagram cookie updater

This tool stores Instagram cookies for the auth-helper.

WHAT TO PASTE
  Paste the full HTTP Cookie header as ONE LINE, for example:

  csrftoken=...; ds_user_id=...; sessionid=...; mid=...; ig_did=...; rur=...

  Minimum required fields:
    sessionid=...
    ds_user_id=...
    csrftoken=...

HOW TO COPY IT FROM CHROME / EDGE
  1. Open instagram.com and log in to the account you want to use.
  2. Open DevTools: F12 or Ctrl+Shift+I.
  3. Go to Network.
  4. Refresh instagram.com.
  5. Click a request to www.instagram.com, for example the document request or /api/ request.
  6. In Headers -> Request Headers, find: Cookie
  7. Copy the WHOLE Cookie value, not just one cookie.

IF YOUR BROWSER ONLY COPIES COOKIES ONE BY ONE
  You must combine them manually into one line:

  name=value; name2=value2; name3=value3

  Example:
  csrftoken=AAA; ds_user_id=123; sessionid=BBB; mid=CCC; ig_did=DDD; rur=...

COOKIE POOL / ROTATION
  Cookies are stored as separate account slots in:
    ${COOKIE_DIR}

  The helper rotates between available accounts conservatively.
  If one account gets login_required/checkpoint/challenge, only that account is cooled down.

Usage if you want direct commands:
  $0 menu                  Show simple menu
  $0 add [slot-name]       Add or replace one account cookie slot
  $0 list                  List stored slots without printing cookie values
  $0 delete [slot-name]    Remove one cookie slot, backed up under deleted/
  $0 health                Show helper health
  $0 test [post-id]        Test authenticated fallback
  $0 test-all [post-id]    Test every cookie slot separately
  $0 legacy                Replace old single-cookie file only

Default action: menu
EOF
}

safe_slot() {
  local raw="${1:-}"
  raw="${raw// /_}"
  raw="$(printf '%s' "$raw" | tr -cd 'A-Za-z0-9_.-')"
  if [[ -z "$raw" ]]; then
    raw="account_$(date -u +%Y%m%dT%H%M%SZ)"
  fi
  printf '%s' "$raw"
}

cookie_slot_files() {
  if [[ ! -d "$COOKIE_DIR" ]]; then
    return 0
  fi
  find "$COOKIE_DIR" -maxdepth 1 -type f -name '*.cookie' -printf '%f\n' | sort || true
}

slot_from_file() {
  local file="$1"
  file="${file##*/}"
  printf '%s' "${file%.cookie}"
}

slot_by_number() {
  local wanted="$1"
  local n=0
  local file
  while IFS= read -r file; do
    [[ -z "$file" ]] && continue
    n=$((n + 1))
    if [[ "$n" == "$wanted" ]]; then
      slot_from_file "$file"
      return 0
    fi
  done < <(cookie_slot_files)
  return 1
}

resolve_slot_selection() {
  local value="${1:-}"
  if [[ "$value" =~ ^[0-9]+$ ]]; then
    slot_by_number "$value"
    return $?
  fi
  safe_slot "$value"
}

extract_cookie_field() {
  local cookie="$1"
  local name="$2"
  printf '%s' "$cookie" | tr ';' '\n' | sed 's/^ *//;s/ *$//' | awk -F= -v n="$name" '$1 == n {print $2; exit}'
}

validate_cookie() {
  local cookie="$1"
  local missing=0
  for key in sessionid ds_user_id csrftoken; do
    if [[ "$cookie" != *"${key}="* ]]; then
      echo "Missing required cookie: ${key}=..." >&2
      missing=1
    fi
  done
  if [[ $missing -ne 0 ]]; then
    return 1
  fi
}

restart_helper() {
  echo "Restarting auth helper..."
  docker restart "$HELPER_CONTAINER" >/dev/null
  echo "Waiting for helper health..."
  for _ in $(seq 1 25); do
    if curl -fsS "$HELPER_HEALTH_URL" >/tmp/instafix-helper-health.json 2>/dev/null; then
      echo "Helper health OK:"
      cat /tmp/instafix-helper-health.json
      echo
      return 0
    fi
    sleep 1
  done
  echo "Helper did not become healthy." >&2
  return 1
}

read_cookie_visible() {
  echo
  echo "Paste the full Cookie header as one line."
  echo "It must look like: csrftoken=...; ds_user_id=...; sessionid=...; mid=..."
  echo 'rur="RVA\054..." with quotes/backslashes is OK. Paste it exactly as copied.'
  echo "Do NOT paste only the sessionid value. Do NOT paste JSON."
  echo "Input is VISIBLE so you can confirm it pasted. It will not be printed again after saving."
  echo
  read -r -p "Instagram Cookie header: " COOKIE
  echo
  COOKIE="$(printf '%s' "$COOKIE" | tr -d '\r\n')"
  if [[ -z "$COOKIE" ]]; then
    echo "No cookie provided." >&2
    exit 1
  fi
  validate_cookie "$COOKIE"
}

add_cookie() {
  local slot="${1:-}"
  read_cookie_visible
  local ds_user_id
  ds_user_id="$(extract_cookie_field "$COOKIE" ds_user_id || true)"
  if [[ -z "$slot" ]]; then
    if [[ -n "$ds_user_id" ]]; then
      slot="ig_${ds_user_id}"
    else
      slot="account_$(date -u +%Y%m%dT%H%M%SZ)"
    fi
  fi
  slot="$(safe_slot "$slot")"
  install -d -m 700 "$COOKIE_DIR"
  local target="${COOKIE_DIR}/${slot}.cookie"
  if [[ -f "$target" ]]; then
    local backup="${target}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p "$target" "$backup"
    chmod 600 "$backup"
    echo "Backup saved: ${backup}"
  fi
  local tmp
  tmp="$(mktemp "${target}.tmp.XXXXXX")"
  printf '%s' "$COOKIE" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$target"
  chmod 600 "$target"
  echo "Cookie slot saved: ${target}"
  if [[ ! -f "$LEGACY_COOKIE_FILE" ]]; then
    printf '%s' "$COOKIE" > "$LEGACY_COOKIE_FILE"
    chmod 600 "$LEGACY_COOKIE_FILE"
    echo "Legacy cookie file also created: ${LEGACY_COOKIE_FILE}"
  fi
  restart_helper
}

legacy_cookie() {
  read_cookie_visible
  install -d -m 700 "$(dirname "$LEGACY_COOKIE_FILE")"
  if [[ -f "$LEGACY_COOKIE_FILE" ]]; then
    local backup="${LEGACY_COOKIE_FILE}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p "$LEGACY_COOKIE_FILE" "$backup"
    chmod 600 "$backup"
    echo "Backup saved: ${backup}"
  fi
  local tmp
  tmp="$(mktemp "${LEGACY_COOKIE_FILE}.tmp.XXXXXX")"
  printf '%s' "$COOKIE" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$LEGACY_COOKIE_FILE"
  chmod 600 "$LEGACY_COOKIE_FILE"
  echo "Legacy cookie updated: ${LEGACY_COOKIE_FILE}"
  restart_helper
}

list_cookies() {
  print_dashboard
}

health() {
  local tmp="/tmp/instafix-helper-health.json"
  if ! curl -fsS "$HELPER_HEALTH_URL" > "$tmp"; then
    echo "Helper health: FAILED"
    return 1
  fi
  python3 - "$tmp" <<'PY'
import json, sys
data = json.load(open(sys.argv[1], encoding="utf-8"))
pool = data.get("cookie_pool") or {}
print("Helper health: OK" if data.get("ok") else "Helper health: NOT OK")
print(f"Cookies: total={pool.get('total', 0)} available={pool.get('available', 0)} cooldown={pool.get('cooling_down', 0)}")
if data.get("auth_circuit_open"):
    print(f"Auth circuit: OPEN, {data.get('auth_circuit_remaining_seconds', 0)}s left, reason={data.get('auth_circuit_reason') or '-'}")
else:
    print("Auth circuit: closed")
PY
}

print_dashboard() {
  echo
  echo "Current cookie accounts"
  echo "-----------------------"
  python3 - "$COOKIE_DIR" "$HELPER_ACCOUNTS_URL" <<'PY'
import json, os, sys, time, urllib.request
cookie_dir, accounts_url = sys.argv[1], sys.argv[2]
files = []
if os.path.isdir(cookie_dir):
    for name in sorted(os.listdir(cookie_dir)):
        if name.endswith(".cookie") and os.path.isfile(os.path.join(cookie_dir, name)):
            path = os.path.join(cookie_dir, name)
            slot = name[:-7]
            mtime = time.strftime("%Y-%m-%d %H:%M", time.localtime(os.path.getmtime(path)))
            files.append((slot, name, mtime))
accounts = {}
try:
    with urllib.request.urlopen(accounts_url, timeout=2) as r:
        payload = json.loads(r.read().decode("utf-8"))
    for item in payload.get("accounts", []):
        accounts[item.get("slot") or item.get("file") or item.get("account_id")] = item
except Exception as exc:
    print(f"Helper status unavailable: {exc}")
if not files:
    print("No cookie accounts yet.")
else:
    for i, (slot, name, mtime) in enumerate(files, 1):
        item = accounts.get(slot) or {}
        if item:
            if item.get("available"):
                status = "OK"
            else:
                status = f"cooldown {int(item.get('cooldown_remaining_seconds') or 0)}s"
        else:
            status = "not loaded"
        print(f"{i}) {slot:<24} {status:<16} {mtime}  ({name})")
print(f"\nDirectory: {cookie_dir}")
PY
}

test_cookie() {
  local post="${1:-$HELPER_TEST_POST}"
  local url="http://127.0.0.1:3200/oembed/${post}"
  local account="${2:-}"
  if [[ -n "$account" ]]; then
    url="${url}?account=${account}&bypass_cache=1"
    echo "Testing slot/account '${account}' with ${post}..."
  else
    echo "Testing authenticated fallback with ${post}..."
    echo "Note: this tests whichever account the helper selects. Use 'test-all' to test every slot."
  fi
  local code
  code="$(curl -sS -o /tmp/instafix-cookie-test.json -w '%{http_code}' "$url" || true)"
  python3 - "$code" /tmp/instafix-cookie-test.json <<'PY'
import json, sys
code, path = sys.argv[1], sys.argv[2]
try:
    data = json.load(open(path, encoding="utf-8"))
except Exception:
    data = {}
ok = code == "200" and data.get("ok") is not False
print(f"HTTP {code} - {'PASS' if ok else 'FAIL'}")
for key in ("slot", "account_id", "username", "media_id"):
    if data.get(key):
        print(f"{key}: {data.get(key)}")
if not ok:
    if data.get("error_code"):
        print(f"error_code: {data.get('error_code')}")
    if data.get("error"):
        print(f"error: {data.get('error')}")
PY
  if [[ "$code" != "200" ]]; then
    echo "Test failed. The cookie may be expired, incomplete, checkpointed, or not allowed for this post." >&2
    return 1
  fi
}

test_all_cookies() {
  local post="${1:-$HELPER_TEST_POST}"
  local count=0
  local ok=0
  while IFS= read -r file; do
    [[ -z "$file" ]] && continue
    count=$((count + 1))
    local slot
    slot="$(slot_from_file "$file")"
    echo
    echo "--- ${count}) ${file} ---"
    if test_cookie "$post" "$slot"; then
      ok=$((ok + 1))
    fi
  done < <(cookie_slot_files)
  echo
  echo "Result: ${ok}/${count} slots passed for ${post}."
  if [[ $count -eq 0 || $ok -eq 0 ]]; then
    return 1
  fi
}

delete_cookie() {
  local slot="${1:-}"
  if [[ -z "$slot" ]]; then
    print_dashboard
    echo
    read -r -p "Delete which account? Type number or name: " slot
  fi
  if [[ -z "$slot" ]]; then
    echo "No slot selected." >&2
    return 1
  fi
  if ! slot="$(resolve_slot_selection "$slot")"; then
    echo "No cookie account with that number." >&2
    return 1
  fi
  local target="${COOKIE_DIR}/${slot}.cookie"
  if [[ ! -f "$target" ]]; then
    echo "Slot not found: ${target}" >&2
    return 1
  fi
  local deleted_dir="${COOKIE_DIR}/deleted"
  install -d -m 700 "$deleted_dir"
  local backup="${deleted_dir}/${slot}.cookie.deleted.$(date -u +%Y%m%dT%H%M%SZ)"
  mv "$target" "$backup"
  chmod 600 "$backup"
  echo "Deleted slot: ${slot}"
  echo "Backup saved: ${backup}"
  restart_helper
}

test_one_cookie() {
  print_dashboard
  echo
  read -r -p "Test which account? Type number or name: " slot
  if [[ -z "$slot" ]]; then
    echo "No slot selected." >&2
    return 1
  fi
  if ! slot="$(resolve_slot_selection "$slot")"; then
    echo "No cookie account with that number." >&2
    return 1
  fi
  read -r -p "Post id [${HELPER_TEST_POST}]: " post
  test_cookie "${post:-$HELPER_TEST_POST}" "$slot"
}

menu() {
  while true; do
    print_dashboard
    echo
    echo "InstaFix Instagram cookie updater"
    echo "=================================="
    echo "1) Add/replace cookie account"
    echo "2) Test all accounts"
    echo "3) Test one account"
    echo "4) Delete account"
    echo "5) Health"
    echo "6) Help: what exactly to paste"
    echo "0) Exit"
    echo
    read -r -p "Choose: " choice
    case "$choice" in
      1)
        echo
        echo "Slot name is optional. Example: acc2"
        echo "If empty, script uses ds_user_id from the cookie."
        read -r -p "Slot name [optional]: " slot
        add_cookie "$slot"
        ;;
      2)
        read -r -p "Post id [${HELPER_TEST_POST}]: " post
        test_all_cookies "${post:-$HELPER_TEST_POST}" || true
        ;;
      3)
        test_one_cookie || true
        ;;
      4)
        delete_cookie || true
        ;;
      5)
        health || true
        ;;
      6)
        usage
        ;;
      0|q|quit|exit)
        exit 0
        ;;
      *)
        echo "Unknown choice."
        ;;
    esac
  done
}

ACTION="${1:-menu}"
case "$ACTION" in
  menu)
    menu
    ;;
  add)
    add_cookie "${2:-}"
    ;;
  list)
    list_cookies
    ;;
  delete|remove|rm)
    delete_cookie "${2:-}"
    ;;
  health)
    health
    ;;
  test)
    test_cookie "${2:-$HELPER_TEST_POST}" "${3:-}"
    ;;
  test-all)
    test_all_cookies "${2:-$HELPER_TEST_POST}"
    ;;
  legacy)
    legacy_cookie
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    echo "Unknown action: ${ACTION}" >&2
    usage >&2
    exit 1
    ;;
esac
