import contextlib
import glob
import hashlib
import hmac
import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, quote, urlparse, urljoin, urlsplit

from curl_cffi import requests


COOKIE_FILE = os.environ.get("INSTAGRAM_COOKIE_FILE", "/run/secrets/instagram_cookie")
COOKIE_DIR = os.environ.get("INSTAGRAM_COOKIE_DIR", "").strip()
COOKIE_POOL_RELOAD_SECONDS = int(os.environ.get("AUTH_HELPER_COOKIE_POOL_RELOAD_SECONDS", "30"))
LISTEN = os.environ.get("AUTH_HELPER_LISTEN", "127.0.0.1:3200")
IMPERSONATE = os.environ.get("AUTH_HELPER_IMPERSONATE", "chrome136")
TIMEOUT = float(os.environ.get("AUTH_HELPER_TIMEOUT_SECONDS", "15"))
MAX_PER_MINUTE = int(os.environ.get("AUTH_HELPER_MAX_PER_MINUTE", "30"))
AUTH_MAX_PER_MINUTE = int(os.environ.get("AUTH_HELPER_AUTH_MAX_PER_MINUTE", "6"))
AUTH_COOLDOWN_SECONDS = int(os.environ.get("AUTH_HELPER_AUTH_COOLDOWN_SECONDS", "21600"))
AUTH_CACHE_TTL_SECONDS = int(os.environ.get("AUTH_HELPER_AUTH_CACHE_TTL_SECONDS", "86400"))
AUTH_NEGATIVE_CACHE_TTL_SECONDS = int(os.environ.get("AUTH_HELPER_AUTH_NEGATIVE_CACHE_TTL_SECONDS", "3600"))
VERIFY_VIDEO_URL = os.environ.get("AUTH_HELPER_VERIFY_VIDEO_URL", "").strip().lower() in ("1", "true", "yes", "on")
FETCH_VIDEO_INFO = os.environ.get("AUTH_HELPER_FETCH_VIDEO_INFO", "").strip().lower() in ("1", "true", "yes", "on")
FETCH_MEDIA_INFO_FALLBACK = os.environ.get("AUTH_HELPER_FETCH_MEDIA_INFO_FALLBACK", "").strip().lower() in ("1", "true", "yes", "on")
SAFE_OEMBED_REDIRECTS = os.environ.get("AUTH_HELPER_SAFE_OEMBED_REDIRECTS", "").strip().lower() in ("1", "true", "yes", "on")
ENABLE_VIDEO_PROXY = os.environ.get("AUTH_HELPER_ENABLE_VIDEO_PROXY", "").strip().lower() in ("1", "true", "yes", "on")
VIDEO_PROXY_SEND_COOKIE = os.environ.get("AUTH_HELPER_VIDEO_PROXY_SEND_COOKIE", "").strip().lower() in ("1", "true", "yes", "on")
VIDEO_PROXY_REFRESH_MODE = os.environ.get("AUTH_HELPER_VIDEO_PROXY_REFRESH_MODE", "on_failure").strip().lower()
VIDEO_PROXY_MAX_BYTES = int(os.environ.get("AUTH_HELPER_VIDEO_PROXY_MAX_BYTES", "50000000"))
VIDEO_PROXY_MAX_CONCURRENT = int(os.environ.get("AUTH_HELPER_VIDEO_PROXY_MAX_CONCURRENT", "1"))
VIDEO_PROXY_TIMEOUT = float(os.environ.get("AUTH_HELPER_VIDEO_PROXY_TIMEOUT_SECONDS", "20"))
VIDEO_PROXY_MAX_RESUME_ATTEMPTS = int(os.environ.get("AUTH_HELPER_VIDEO_PROXY_MAX_RESUME_ATTEMPTS", "256"))
VIDEO_PROXY_UPSTREAM_CHUNK_BYTES = int(os.environ.get("AUTH_HELPER_VIDEO_PROXY_UPSTREAM_CHUNK_BYTES", "262144"))
VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT = float(os.environ.get("AUTH_HELPER_VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT_SECONDS", "6"))
AUTH_JSON_MAX_BYTES = int(os.environ.get("AUTH_HELPER_JSON_MAX_BYTES", "1048576"))
AUTH_CACHE_MAX_ENTRIES = int(os.environ.get("AUTH_HELPER_CACHE_MAX_ENTRIES", "512"))
COOKIE_ADMIN_TOKEN = os.environ.get("COOKIE_ADMIN_TOKEN", "").strip()
COOKIE_ADMIN_MAX_BYTES = int(os.environ.get("COOKIE_ADMIN_MAX_BYTES", "65536"))
COOKIE_STATE_FILE = os.environ.get("AUTH_HELPER_COOKIE_STATE_FILE", "").strip()
COOKIE_VALIDATION_POST_ID = os.environ.get("AUTH_HELPER_COOKIE_VALIDATION_POST_ID", "DW9rpAdCeCK").strip()
POST_ID_RE = re.compile(r"^[A-Za-z0-9_-]{6,32}$")
COOKIE_SLOT_RE = re.compile(r"^[A-Za-z0-9_.-]{1,80}$")
SHORTCODE_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
VIDEO_PROXY_ALLOWED_HOST_SUFFIXES = (".cdninstagram.com", ".fbcdn.net")
CONTENT_RANGE_RE = re.compile(r"^bytes\s+(\d+)-(\d+)/(\d+|\*)$", re.IGNORECASE)
REQUEST_RANGE_RE = re.compile(r"^bytes=(\d+)-(\d*)$", re.IGNORECASE)

rate_lock = threading.Lock()
rate_window = int(time.time() // 60)
rate_count = 0
auth_rate_window = int(time.time() // 60)
auth_rate_count = 0
auth_state_lock = threading.Lock()
auth_cooldown_until = 0.0
auth_cooldown_reason = ""
auth_cache_lock = threading.Lock()
auth_success_cache = {}
auth_negative_cache = {}
account_history_lock = threading.Lock()
account_history = {}
cookie_pool_lock = threading.Lock()
cookie_pool = []
cookie_pool_loaded_at = 0.0
cookie_cursor = 0
account_cooldowns = {}
account_quarantines = {}
account_quarantines_loaded = False
video_proxy_semaphore = threading.BoundedSemaphore(max(1, VIDEO_PROXY_MAX_CONCURRENT))

AUTH_CIRCUIT_CODES = {"login_required", "checkpoint_required", "challenge_required", "cookie_missing", "session_invalid"}
ACCOUNT_QUARANTINE_CODES = {"login_required", "checkpoint_required", "challenge_required", "cookie_missing", "session_invalid"}
COOKIE_MEDIA_PROBE_ACCEPTED_RESTRICTIONS = {"geoblock_required", "private_media"}
REDIRECT_STATUSES = {301, 302, 303, 307, 308}


class HelperError(Exception):
    def __init__(self, code, message, status=None, cached=False):
        super().__init__(message)
        self.code = code
        self.status = status
        self.cached = cached


class CookieAccount:
    def __init__(self, account_id, cookie, source):
        self.account_id = account_id
        self.cookie = cookie
        self.source = source


def log(level, msg, **fields):
    fields.update({"level": level, "msg": msg, "time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())})
    print(json.dumps(fields, ensure_ascii=False), flush=True)


def load_cookie():
    with open(COOKIE_FILE, "r", encoding="utf-8-sig") as f:
        return f.read().strip().replace("\r", "").replace("\n", "")


def normalize_cookie(raw):
    return str(raw or "").strip().replace("\r", "").replace("\n", "")


def cookie_account_id(cookie, source=""):
    ds_match = re.search(r"(?:^|;\s*)ds_user_id=([^;]+)", cookie)
    if ds_match:
        return "ig_" + re.sub(r"[^A-Za-z0-9_.-]", "_", ds_match.group(1))[:80]
    digest = hashlib.sha256((source + "\0" + cookie).encode("utf-8", "ignore")).hexdigest()[:16]
    return "cookie_" + digest


def read_cookie_file(path):
    with open(path, "r", encoding="utf-8-sig") as f:
        cookie = normalize_cookie(f.read())
    if "sessionid=" not in cookie:
        raise HelperError("cookie_missing", f"cookie file {path} does not contain sessionid")
    return cookie


def discover_cookie_accounts(force=False):
    global cookie_pool_loaded_at, cookie_pool
    now = time.time()
    with cookie_pool_lock:
        if not force and cookie_pool and now - cookie_pool_loaded_at < max(1, COOKIE_POOL_RELOAD_SECONDS):
            return list(cookie_pool)

        paths = []
        if COOKIE_DIR:
            for pattern in ("*.cookie", "*.txt", "instagram_cookie*"):
                paths.extend(glob.glob(os.path.join(COOKIE_DIR, pattern)))
        # Compatibility fallback only. If a cookie pool directory has slots,
        # do not also include the old single-cookie file because that makes
        # list/delete UX confusing and can keep a stale legacy account alive.
        if COOKIE_FILE and not paths:
            paths.append(COOKIE_FILE)
        unique_paths = []
        seen_paths = set()
        for path in paths:
            path = os.path.abspath(path)
            if path in seen_paths or not os.path.isfile(path):
                continue
            seen_paths.add(path)
            unique_paths.append(path)

    accounts = []
    seen_accounts = set()
    for path in sorted(unique_paths):
        try:
            cookie = read_cookie_file(path)
        except Exception as exc:
            log("warn", "cookie account skipped", source=path, error=str(exc)[:160])
            continue
        account_id = cookie_account_id(cookie, path)
        if account_id in seen_accounts:
            continue
        seen_accounts.add(account_id)
        accounts.append(CookieAccount(account_id, cookie, path))
    with cookie_pool_lock:
        cookie_pool = accounts
        cookie_pool_loaded_at = now
    return list(accounts)


def cookie_account_status(account_id):
    with cookie_pool_lock:
        remaining = int(max(0, account_cooldowns.get(account_id, 0) - time.time()))
    return remaining


def load_account_quarantines():
    global account_quarantines_loaded, account_quarantines
    with cookie_pool_lock:
        if account_quarantines_loaded:
            return dict(account_quarantines)
        loaded = {}
        if COOKIE_STATE_FILE:
            try:
                with open(COOKIE_STATE_FILE, "r", encoding="utf-8") as f:
                    payload = json.load(f)
                for account_id, item in (payload.get("needs_login") or {}).items():
                    if account_id:
                        loaded[str(account_id)] = {
                            "code": str((item or {}).get("code") or "session_invalid")[:80],
                            "at": str((item or {}).get("at") or "")[:80],
                        }
            except FileNotFoundError:
                pass
            except Exception as exc:
                log("warn", "cookie state load failed", error=str(exc)[:160])
        account_quarantines = loaded
        account_quarantines_loaded = True
        return dict(account_quarantines)


def persist_account_quarantines():
    if not COOKIE_STATE_FILE:
        return
    with cookie_pool_lock:
        payload = {"needs_login": dict(account_quarantines)}
    directory = os.path.dirname(COOKIE_STATE_FILE)
    if directory:
        os.makedirs(directory, mode=0o700, exist_ok=True)
    tmp = COOKIE_STATE_FILE + ".tmp." + str(os.getpid())
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.chmod(tmp, 0o600)
    os.replace(tmp, COOKIE_STATE_FILE)


def account_needs_login(account_id):
    return account_id in load_account_quarantines()


def quarantine_account(account_id, code):
    if not account_id or code not in ACCOUNT_QUARANTINE_CODES:
        return
    load_account_quarantines()
    with cookie_pool_lock:
        account_quarantines[account_id] = {"code": code, "at": utc_now_text()}
    persist_account_quarantines()


def clear_account_quarantine(account_id):
    if not account_id:
        return
    load_account_quarantines()
    changed = False
    with cookie_pool_lock:
        if account_id in account_quarantines:
            account_quarantines.pop(account_id, None)
            changed = True
    if changed:
        persist_account_quarantines()


def cookie_account_slot(account):
    name = os.path.basename(account.source or account.account_id)
    for suffix in (".cookie", ".txt"):
        if name.endswith(suffix):
            return name[: -len(suffix)]
    return name


def cookie_accounts_status():
    accounts = discover_cookie_accounts(force=True)
    items = []
    for account in accounts:
        cooldown = cookie_account_status(account.account_id)
        needs_login = account_needs_login(account.account_id)
        history = account_history_snapshot(account.account_id)
        items.append({
            "account_id": account.account_id,
            "slot": cookie_account_slot(account),
            "file": os.path.basename(account.source or ""),
            "available": cooldown <= 0 and not needs_login,
            "needs_login": needs_login,
            "cooldown_remaining_seconds": cooldown,
            "username": history.get("username", ""),
            "last_success_at": history.get("last_success_at", ""),
            "last_failure_at": history.get("last_failure_at", ""),
            "last_error_code": history.get("last_error_code", ""),
            "last_error_message": history.get("last_error_message", ""),
            "consecutive_failures": history.get("consecutive_failures", 0),
            "last_test_at": history.get("last_test_at", ""),
            "last_test_code": history.get("last_test_code", ""),
        })
    return items


def utc_now_text():
    return time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime())


def account_history_snapshot(account_id):
    with account_history_lock:
        return dict(account_history.get(account_id, {}))


def record_account_success(account, username="", source="auth"):
    if account is None or not account.account_id:
        return
    with account_history_lock:
        item = dict(account_history.get(account.account_id, {}))
        item["slot"] = cookie_account_slot(account)
        item["file"] = os.path.basename(account.source or "")
        item["last_success_at"] = utc_now_text()
        item["last_success_source"] = source
        item["last_error_code"] = ""
        item["last_error_message"] = ""
        item["consecutive_failures"] = 0
        if username:
            item["username"] = str(username)[:80]
        account_history[account.account_id] = item


def record_account_failure(account, code, message="", source="auth"):
    account_id = account.account_id if isinstance(account, CookieAccount) else str(account or "")
    if not account_id:
        return
    with account_history_lock:
        item = dict(account_history.get(account_id, {}))
        if isinstance(account, CookieAccount):
            item["slot"] = cookie_account_slot(account)
            item["file"] = os.path.basename(account.source or "")
        item["last_failure_at"] = utc_now_text()
        item["last_failure_source"] = source
        item["last_error_code"] = str(code or "auth_failed")[:80]
        item["last_error_message"] = str(message or "")[:200]
        item["consecutive_failures"] = int(item.get("consecutive_failures") or 0) + 1
        account_history[account_id] = item


def record_account_test(account, code, username="", message=""):
    if account is None or not account.account_id:
        return
    with account_history_lock:
        item = dict(account_history.get(account.account_id, {}))
        item["slot"] = cookie_account_slot(account)
        item["file"] = os.path.basename(account.source or "")
        item["last_test_at"] = utc_now_text()
        item["last_test_code"] = str(code or "")[:80]
        if username:
            item["username"] = str(username)[:80]
        if code != "ok":
            item["last_error_code"] = str(code or "test_failed")[:80]
            item["last_error_message"] = str(message or "")[:200]
            item["last_failure_at"] = item["last_test_at"]
            item["last_failure_source"] = "homepage_test"
            item["consecutive_failures"] = int(item.get("consecutive_failures") or 0) + 1
        else:
            item["last_success_at"] = item["last_test_at"]
            item["last_success_source"] = "homepage_test"
            item["last_error_code"] = ""
            item["last_error_message"] = ""
            item["consecutive_failures"] = 0
        account_history[account.account_id] = item


def clear_auth_cooldown():
    global auth_cooldown_until, auth_cooldown_reason
    with auth_state_lock:
        auth_cooldown_until = 0.0
        auth_cooldown_reason = ""


def clear_cookie_cooldown(account_id):
    if not account_id:
        return
    with cookie_pool_lock:
        account_cooldowns.pop(account_id, None)


def cookie_path_for_slot(slot):
    slot = str(slot or "").strip()
    if not COOKIE_SLOT_RE.match(slot):
        raise HelperError("invalid_slot", "invalid cookie slot")
    account = find_cookie_account(slot)
    if account is not None and account.source:
        return account.source, account.account_id
    if not COOKIE_DIR:
        raise HelperError("cookie_dir_missing", "cookie pool directory is not configured")
    filename = slot if slot.endswith((".cookie", ".txt")) else slot + ".cookie"
    path = os.path.abspath(os.path.join(COOKIE_DIR, filename))
    base = os.path.abspath(COOKIE_DIR)
    if not (path == base or path.startswith(base + os.sep)):
        raise HelperError("invalid_slot", "invalid cookie slot path")
    return path, ""


def normalize_instagram_username(value):
    return str(value or "").strip().lstrip("@").lower()


def update_cookie_slot(slot, raw_cookie, expected_username=""):
    cookie = normalize_cookie(raw_cookie)
    if not cookie:
        raise HelperError("cookie_missing", "cookie is empty")
    if len(cookie.encode("utf-8", "ignore")) > COOKIE_ADMIN_MAX_BYTES:
        raise HelperError("cookie_too_large", "cookie is too large")
    if "sessionid=" not in cookie:
        raise HelperError("cookie_missing", "cookie does not contain sessionid")
    path, old_account_id = cookie_path_for_slot(slot)
    candidate = CookieAccount(cookie_account_id(cookie, path), cookie, path)
    validation = test_cookie_account(candidate, quarantine_on_failure=False)
    expected = normalize_instagram_username(expected_username)
    actual = normalize_instagram_username(validation.get("username"))
    if expected and actual != expected:
        raise HelperError(
            "account_mismatch",
            f"logged in as @{actual or 'unknown'}, but slot {slot} expects @{expected}",
        )
    directory = os.path.dirname(path)
    os.makedirs(directory, exist_ok=True)
    tmp = path + ".tmp." + str(os.getpid()) + "." + str(int(time.time() * 1000))
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(cookie)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    if old_account_id:
        clear_cookie_cooldown(old_account_id)
        clear_account_quarantine(old_account_id)
    accounts = discover_cookie_accounts(force=True)
    updated = find_cookie_account(slot)
    if updated is not None:
        clear_cookie_cooldown(updated.account_id)
        clear_account_quarantine(updated.account_id)
    clear_auth_cooldown()
    return {
        "slot": cookie_account_slot(updated) if updated is not None else slot,
        "account_id": updated.account_id if updated is not None else "",
        "file": os.path.basename(path),
        "validation": validation,
        "pool": {"total": len(accounts), "available": cookie_pool_health()["available"], "cooling_down": cookie_pool_health()["cooling_down"]},
    }


def find_cookie_account(account_ref):
    wanted = str(account_ref or "").strip()
    if not wanted:
        return None
    for account in discover_cookie_accounts(force=True):
        names = {
            account.account_id,
            cookie_account_slot(account),
            os.path.basename(account.source or ""),
        }
        if wanted in names:
            return account
    return None


def mark_account_failure(account_id, code, post_id=""):
    if code not in AUTH_CIRCUIT_CODES or not account_id:
        return
    until = time.time() + max(60, AUTH_COOLDOWN_SECONDS)
    with cookie_pool_lock:
        account_cooldowns[account_id] = until
    quarantine_account(account_id, code)
    record_account_failure(account_id, code, f"cooldown opened after {post_id}" if post_id else "cooldown opened", "cooldown")
    log("warn", "cookie account cooldown opened", account_id=account_id, post_id=post_id, error_code=code, cooldown_seconds=max(60, AUTH_COOLDOWN_SECONDS))


def choose_cookie_account(post_id=""):
    global cookie_cursor
    accounts = discover_cookie_accounts()
    if not accounts:
        raise HelperError("cookie_missing", "no usable Instagram cookie files found")
    eligible = [account for account in accounts if not account_needs_login(account.account_id)]
    if not eligible:
        raise HelperError("cookie_missing", "all Instagram cookie accounts need login recovery")
    now = time.time()
    with cookie_pool_lock:
        total = len(eligible)
        for _ in range(total):
            idx = cookie_cursor % total
            cookie_cursor += 1
            account = eligible[idx]
            if account_cooldowns.get(account.account_id, 0) <= now:
                return account
        soonest = min(account_cooldowns.get(account.account_id, 0) for account in eligible)
    remaining = int(max(1, soonest - now))
    raise HelperError("auth_circuit_open", f"all Instagram cookie accounts are cooling down for {remaining}s")


def cookie_pool_health():
    accounts = discover_cookie_accounts()
    healthy = 0
    cooling = 0
    needs_login = 0
    for account in accounts:
        if account_needs_login(account.account_id):
            needs_login += 1
        elif cookie_account_status(account.account_id) > 0:
            cooling += 1
        else:
            healthy += 1
    return {"total": len(accounts), "available": healthy, "cooling_down": cooling, "needs_login": needs_login}


def classify_instagram_error(status, body, fallback="instagram_error"):
    text = body[:4096].decode("utf-8", "ignore").lower() if isinstance(body, (bytes, bytearray)) else str(body or "").lower()
    try:
        payload = json.loads(body)
        if isinstance(payload, dict):
            text += " " + " ".join(str(payload.get(k, "")) for k in ("message", "error", "error_type", "status"))
    except Exception:
        pass

    if "checkpoint_required" in text:
        return "checkpoint_required"
    if "challenge_required" in text or "challenge" in text:
        return "challenge_required"
    if "login_required" in text or "require_login" in text:
        return "login_required"
    if "geoblock_required" in text or "geoblock" in text:
        return "geoblock_required"
    if status in (401, 403) and ("not-logged-in" in text or "login" in text):
        return "login_required"
    if "private media" in text:
        return "private_media"
    if "please wait a few minutes" in text or status == 429:
        return "rate_limited"
    if status == 401:
        return "login_required"
    if status == 403:
        return "auth_forbidden"
    if status == 404:
        return "media_not_found"
    return fallback


def classify_redirect_location(location):
    text = str(location or "").lower()
    if "/accounts/login" in text or "login" in text:
        return "login_required"
    if "/challenge" in text or "challenge" in text:
        return "challenge_required"
    if "/checkpoint" in text or "checkpoint" in text:
        return "checkpoint_required"
    return "redirected"


def mark_auth_failure(code, post_id=""):
    global auth_cooldown_until, auth_cooldown_reason
    if code not in AUTH_CIRCUIT_CODES:
        return
    until = time.time() + max(60, AUTH_COOLDOWN_SECONDS)
    with auth_state_lock:
        if until > auth_cooldown_until:
            auth_cooldown_until = until
            auth_cooldown_reason = code
    log("warn", "auth circuit breaker opened", post_id=post_id, error_code=code, cooldown_seconds=max(60, AUTH_COOLDOWN_SECONDS))


def auth_circuit_status():
    with auth_state_lock:
        remaining = int(max(0, auth_cooldown_until - time.time()))
        reason = auth_cooldown_reason
    return remaining, reason


def require_auth_available(post_id=""):
    remaining, reason = auth_circuit_status()
    if remaining > 0:
        raise HelperError("auth_circuit_open", f"authenticated requests paused for {remaining}s after {reason}")


def allow_auth_request():
    global auth_rate_window, auth_rate_count
    now = int(time.time() // 60)
    with rate_lock:
        if now != auth_rate_window:
            auth_rate_window = now
            auth_rate_count = 0
        if auth_rate_count >= AUTH_MAX_PER_MINUTE:
            return False
        auth_rate_count += 1
        return True



def same_origin_oembed_redirect(current_url, location):
    """Return a cookie-safe Instagram oEmbed redirect target or None.

    Redirects to a different origin, protocol, port, or API family are
    rejected. In particular, never forward a session cookie based on an
    untrusted absolute Location header.
    """
    try:
        current = urlsplit(current_url)
        target = urlsplit(urljoin(current_url, location))
        if not (
            current.scheme == "https"
            and current.hostname == "www.instagram.com"
            and current.port in (None, 443)
            and target.scheme == "https"
            and target.hostname == current.hostname
            and target.port in (None, 443)
            and target.username is None
            and target.password is None
            and target.path.startswith("/api/v1/oembed/")
        ):
            return None
        return target.geturl()
    except (ValueError, TypeError):
        return None


def auth_get(url, headers, post_id="", account=None, timeout=None, stream=False, mark_account_failures=True):
    require_auth_available(post_id)
    if not allow_auth_request():
        raise HelperError("auth_rate_limited", "authenticated request rate limited", 429)
    response = requests.get(url, headers=headers, impersonate=IMPERSONATE, timeout=timeout or TIMEOUT, allow_redirects=False, stream=stream)
    if response.status_code in REDIRECT_STATUSES:
        location = response.headers.get("location", "")
        code = classify_redirect_location(location)
        if SAFE_OEMBED_REDIRECTS and "/api/v1/oembed/" in url:
            current_url = url
            visited = {url}
            # At most two additional authenticated requests; each consumes
            # the normal auth rate budget and remains on the exact same host.
            for _ in range(2):
                if code != "redirected":
                    break
                next_url = same_origin_oembed_redirect(current_url, location)
                if not next_url or next_url in visited:
                    break
                if not allow_auth_request():
                    with contextlib.suppress(Exception):
                        response.close()
                    raise HelperError("auth_rate_limited", "authenticated redirect rate limited", 429)
                visited.add(next_url)
                current_url = next_url
                with contextlib.suppress(Exception):
                    response.close()
                response = requests.get(next_url, headers=headers, impersonate=IMPERSONATE, timeout=timeout or TIMEOUT, allow_redirects=False, stream=stream)
                if response.status_code not in REDIRECT_STATUSES:
                    return response
                location = response.headers.get("location", "")
                code = classify_redirect_location(location)
        if mark_account_failures and code in AUTH_CIRCUIT_CODES:
            if account is not None:
                mark_account_failure(account.account_id, code, post_id)
            else:
                mark_auth_failure(code, post_id)
        with contextlib.suppress(Exception):
            response.close()
        raise HelperError(code, f"Instagram redirected to {location[:160] or 'unknown'}", response.status_code)
    return response


def bounded_response_bytes(response, limit=AUTH_JSON_MAX_BYTES):
    limit = max(1, int(limit or 1))
    chunks = []
    total = 0
    try:
        for chunk in response.iter_content(chunk_size=min(64 * 1024, limit)):
            if not chunk:
                continue
            total += len(chunk)
            if total > limit:
                raise HelperError("response_too_large", f"upstream response exceeded {limit} bytes")
            chunks.append(chunk)
    finally:
        with contextlib.suppress(Exception):
            response.close()
    return b"".join(chunks)


def bounded_stream_prefix(response, limit=1024):
    limit = max(1, int(limit or 1))
    chunks = []
    total = 0
    try:
        for chunk in response.iter_content(chunk_size=min(16 * 1024, limit)):
            if not chunk:
                continue
            remaining = limit - total
            if remaining <= 0:
                break
            chunks.append(chunk[:remaining])
            total += min(len(chunk), remaining)
            if total >= limit:
                break
    finally:
        with contextlib.suppress(Exception):
            response.close()
    return b"".join(chunks)


def cache_get(cache, key):
    now = time.time()
    with auth_cache_lock:
        item = cache.get(key)
        if not item:
            return None
        expires, value = item
        if expires <= now:
            cache.pop(key, None)
            return None
        return value


def cache_set(cache, key, value, ttl):
    if ttl <= 0:
        return
    with auth_cache_lock:
        if len(cache) >= max(1, AUTH_CACHE_MAX_ENTRIES):
            now = time.time()
            expired = [k for k, item in cache.items() if item[0] <= now]
            for expired_key in expired:
                cache.pop(expired_key, None)
            while len(cache) >= max(1, AUTH_CACHE_MAX_ENTRIES):
                oldest_key = min(cache, key=lambda k: cache[k][0])
                cache.pop(oldest_key, None)
        cache[key] = (time.time() + ttl, value)


def cache_delete(cache, key):
    with auth_cache_lock:
        cache.pop(key, None)


def cache_negative(post_id, code, message, account_id=""):
    cache_set(auth_negative_cache, auth_negative_key(post_id, account_id), {"code": code or "auth_helper_failed", "message": str(message or "")[:300]}, AUTH_NEGATIVE_CACHE_TTL_SECONDS)


def auth_negative_key(post_id, account_id=""):
    return f"{post_id}:{account_id or 'global'}"


def cached_auth_payload(post_id, account_id=""):
    negative = cache_get(auth_negative_cache, auth_negative_key(post_id, account_id)) if account_id else None
    if negative:
        log("info", "auth oembed failure served from cache", post_id=post_id, error_code=negative.get("code") or "auth_negative_cached")
        raise HelperError(
            negative.get("code") or "auth_negative_cached",
            negative.get("message") or "cached authenticated failure",
            cached=True,
        )
    cached = cache_get(auth_success_cache, post_id)
    if cached:
        log("info", "auth oembed served from cache", post_id=post_id)
        payload = dict(cached)
        payload["cache_hit"] = True
        return payload
    return None


def store_auth_payload(post_id, payload):
    if payload and payload.get("ok"):
        cache_set(auth_success_cache, post_id, dict(payload), AUTH_CACHE_TTL_SECONDS)


def classify_exception(exc):
    if isinstance(exc, HelperError):
        return exc.code
    text = str(exc).lower()
    if "maximum" in text and "redirect" in text:
        return "login_required"
    if "login_required" in text or "require_login" in text:
        return "login_required"
    if "checkpoint_required" in text:
        return "checkpoint_required"
    if "challenge_required" in text or "challenge" in text:
        return "challenge_required"
    if "cookie file" in text or "sessionid" in text:
        return "cookie_missing"
    return "auth_helper_failed"


def allow_request():
    global rate_window, rate_count
    now = int(time.time() // 60)
    with rate_lock:
        if now != rate_window:
            rate_window = now
            rate_count = 0
        if rate_count >= MAX_PER_MINUTE:
            return False
        rate_count += 1
        return True


def oembed(post_id, forced_account=None, bypass_cache=False):
    if forced_account is None and not bypass_cache:
        cached = cached_auth_payload(post_id)
        if cached:
            return cached
    elif forced_account is not None and not bypass_cache:
        cached_auth_payload(post_id, forced_account.account_id)
    account = forced_account or choose_cookie_account(post_id)
    account_id = account.account_id
    if forced_account is None and not bypass_cache:
        cached = cached_auth_payload(post_id, account_id)
        if cached:
            return cached
    cookie = account.cookie
    post_url = f"https://www.instagram.com/reel/{post_id}/"
    url = "https://www.instagram.com/api/v1/oembed/?url=" + quote(post_url, safe="")
    headers = {
        "accept": "application/json,text/html,*/*",
        "accept-language": "ru,en-US;q=0.9,en;q=0.8,uk;q=0.7",
        "cookie": cookie,
        "referer": post_url,
        "x-ig-app-id": "936619743392459",
        "x-asbd-id": "129477",
        "x-requested-with": "XMLHttpRequest",
    }
    try:
        r = auth_get(url, headers, post_id=post_id, account=account, stream=True)
    except Exception as exc:
        code = classify_exception(exc)
        record_account_failure(account, code, exc, "oembed")
        cache_negative(post_id, code, exc, account_id)
        raise
    try:
        body = bounded_response_bytes(r)
    except HelperError as exc:
        record_account_failure(account, exc.code, exc, "oembed")
        cache_negative(post_id, exc.code, exc, account_id)
        raise
    try:
        data = json.loads(body.decode("utf-8"))
    except Exception:
        data = {}
    if r.status_code != 200:
        message = str((data or {}).get("message") or "") if isinstance(data, dict) else ""
        if not message and b"5xx Server Error" in body:
            message = "instagram edge 5xx"
        code = classify_instagram_error(r.status_code, body, "auth_oembed_unavailable")
        if code in AUTH_CIRCUIT_CODES:
            mark_account_failure(account.account_id, code, post_id)
        if not FETCH_MEDIA_INFO_FALLBACK:
            record_account_failure(account, code, f"Instagram oEmbed HTTP {r.status_code}: {message}", "oembed")
            cache_negative(post_id, code, f"Instagram oEmbed HTTP {r.status_code}: {message}", account_id)
            raise HelperError(code, f"Instagram oEmbed HTTP {r.status_code}: {message or r.status_code}", r.status_code)
        log("warn", "auth oembed unavailable; trying media info fallback", post_id=post_id, status=r.status_code, error=message[:120])
        try:
            payload = media_info_payload(post_id, str(shortcode_to_media_id(post_id)), account, post_url)
            store_auth_payload(post_id, payload)
            return payload
        except HelperError as exc:
            if exc.code in AUTH_CIRCUIT_CODES:
                record_account_failure(account, exc.code, exc, "oembed")
                cache_negative(post_id, exc.code, exc, account_id)
                raise
            if code == "geoblock_required":
                error = f"Instagram oEmbed HTTP {r.status_code}: geoblock_required; media info fallback: {exc}"
                record_account_failure(account, code, error, "oembed")
                cache_negative(post_id, code, error, account_id)
                raise HelperError(code, error, r.status_code)
            if exc.code == "private_media":
                code = classify_instagram_error(r.status_code, body, "auth_forbidden")
                record_account_failure(account, code, f"Instagram oEmbed HTTP {r.status_code}: {message}", "oembed")
                cache_negative(post_id, code, f"Instagram oEmbed HTTP {r.status_code}: {message}", account_id)
                raise HelperError(code, f"Instagram oEmbed HTTP {r.status_code}: {message}", r.status_code)
            record_account_failure(account, exc.code, exc, "oembed")
            cache_negative(post_id, exc.code, exc, account_id)
            raise
    if not isinstance(data, dict):
        raise HelperError("invalid_response", "Instagram oEmbed response is not a JSON object")
    thumbnail = str(data.get("thumbnail_url") or "")
    parsed = urlparse(thumbnail)
    if parsed.scheme not in ("http", "https") or not parsed.netloc:
        raise HelperError("invalid_response", "Instagram oEmbed response has no valid thumbnail_url")
    author = str(data.get("author_name") or "").strip().lstrip("@")
    title = str(data.get("title") or "").strip()
    media_id = str(data.get("media_id") or "").strip()
    video_url = ""
    width = 0
    height = 0
    if media_id and FETCH_VIDEO_INFO:
        try:
            video_url, width, height = media_info_video_url(media_id, account, post_url)
        except Exception as exc:
            log("warn", "auth media info video lookup failed", post_id=post_id, error=str(exc)[:300])
    elif media_id:
        log("info", "auth media info video lookup skipped", post_id=post_id, reason="disabled")
    if not author:
        author = "instagram"
    payload = {
        "ok": True,
        "post_id": post_id,
        "username": author,
        "caption": title,
        "thumbnail_url": thumbnail,
        "video_url": video_url,
        "width": width,
        "height": height,
        "media_id": media_id,
    }
    record_account_success(account, author, "oembed")
    store_auth_payload(post_id, payload)
    return payload


def extract_homepage_username(text):
    if not text:
        return ""
    patterns = [
        r'"username"\s*:\s*"([A-Za-z0-9_.]{1,80})"',
        r'"viewer"\s*:\s*\{[^{}]{0,2000}"username"\s*:\s*"([A-Za-z0-9_.]{1,80})"',
        r'"viewer_username"\s*:\s*"([A-Za-z0-9_.]{1,80})"',
    ]
    for pattern in patterns:
        m = re.search(pattern, text)
        if m:
            return m.group(1)
    return ""


def extract_cookie_value(cookie, name):
    prefix = str(name or "") + "="
    for part in str(cookie or "").split(";"):
        part = part.strip()
        if part.startswith(prefix):
            return part[len(prefix):].strip()
    return ""


def extract_username_from_json(value, depth=0):
    if depth > 8:
        return ""
    if isinstance(value, dict):
        for key in ("username", "viewer_username"):
            candidate = str(value.get(key) or "").strip().lstrip("@")
            if re.match(r"^[A-Za-z0-9_.]{1,80}$", candidate):
                return candidate
        for key in ("user", "viewer", "current_user", "data", "config", "props", "graphql"):
            if key in value:
                candidate = extract_username_from_json(value.get(key), depth + 1)
                if candidate:
                    return candidate
        for child in value.values():
            candidate = extract_username_from_json(child, depth + 1)
            if candidate:
                return candidate
    elif isinstance(value, list):
        for child in value[:50]:
            candidate = extract_username_from_json(child, depth + 1)
            if candidate:
                return candidate
    return ""


def fetch_current_username(account):
    """Best-effort username lookup for the manual admin cookie test only."""
    csrf = extract_cookie_value(account.cookie, "csrftoken")
    headers = {
        "accept": "application/json,text/plain,*/*",
        "accept-language": "ru,en-US;q=0.9,en;q=0.8,uk;q=0.7",
        "cookie": account.cookie,
        "referer": "https://www.instagram.com/",
        "x-ig-app-id": "936619743392459",
        "x-requested-with": "XMLHttpRequest",
    }
    if csrf:
        headers["x-csrftoken"] = csrf
    urls = (
        "https://www.instagram.com/api/v1/accounts/current_user/?edit=true",
        "https://www.instagram.com/api/v1/web/data/shared_data/",
        "https://www.instagram.com/api/v1/web/accounts/web_change_profile_picture/",
    )
    for url in urls:
        r = None
        try:
            r = requests.get(url, headers=headers, impersonate=IMPERSONATE, timeout=min(TIMEOUT, 10), allow_redirects=False, stream=True)
            if r.status_code in REDIRECT_STATUSES:
                log("info", "current username lookup redirected", slot=cookie_account_slot(account), code=classify_redirect_location(r.headers.get("location", "")))
                continue
            body = bounded_response_bytes(r, 256 * 1024)
            r = None
            if r is not None:
                with contextlib.suppress(Exception):
                    r.close()
            if not body:
                continue
            try:
                payload = json.loads(body.decode("utf-8", "ignore"))
            except Exception:
                continue
            username = extract_username_from_json(payload)
            if username:
                return username
        except Exception as exc:
            log("info", "current username lookup failed", slot=cookie_account_slot(account), error=str(exc)[:160])
        finally:
            if r is not None:
                with contextlib.suppress(Exception):
                    r.close()
    return ""


def test_cookie_homepage(account_ref):
    account = find_cookie_account(account_ref)
    if account is None:
        raise HelperError("account_not_found", "cookie account not found")
    return test_cookie_account(account)


def test_cookie_account(account, quarantine_on_failure=True):
    url = "https://www.instagram.com/"
    headers = {
        "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "accept-language": "ru,en-US;q=0.9,en;q=0.8,uk;q=0.7",
        "cookie": account.cookie,
        "referer": "https://www.instagram.com/",
    }
    require_auth_available("homepage_test")
    if not allow_auth_request():
        raise HelperError("auth_rate_limited", "authenticated request rate limited", 429)
    body = b""
    r = None
    try:
        r = requests.get(url, headers=headers, impersonate=IMPERSONATE, timeout=min(TIMEOUT, 10), allow_redirects=True, stream=True)
        body = bounded_stream_prefix(r, 128 * 1024)
        status = r.status_code
    except Exception as exc:
        code = classify_exception(exc)
        record_account_test(account, code, message=str(exc))
        if quarantine_on_failure:
            quarantine_account(account.account_id, code)
        raise
    finally:
        if r is not None:
            with contextlib.suppress(Exception):
                r.close()
    code = classify_instagram_error(status, body, "homepage_unavailable") if status != 200 else "ok"
    text = body.decode("utf-8", "ignore")
    username = extract_homepage_username(text)
    final_url = str(getattr(r, "url", "") or "").lower()
    if status != 200:
        record_account_test(account, code, username=username, message=f"homepage HTTP {status}")
        if quarantine_on_failure:
            quarantine_account(account.account_id, code)
        raise HelperError(code, f"homepage HTTP {status}", status)
    lower = text.lower()
    if "/accounts/login" in final_url or "/accounts/login" in lower or "login_required" in lower or "checkpoint_required" in lower or "/challenge" in final_url or "challenge_required" in lower:
        code = "login_required" if "/accounts/login" in final_url or "/accounts/login" in lower or "login_required" in lower else classify_instagram_error(status, body, "homepage_login_wall")
        record_account_test(account, code, username=username, message="homepage returned login/checkpoint wall")
        if quarantine_on_failure:
            quarantine_account(account.account_id, code)
        raise HelperError(code, "homepage returned login/checkpoint wall", status)
    if not username:
        username = fetch_current_username(account)
    if not username:
        code = "session_invalid"
        record_account_test(account, code, message="authenticated identity could not be confirmed")
        if quarantine_on_failure:
            quarantine_account(account.account_id, code)
        raise HelperError(code, "authenticated identity could not be confirmed", status)

    # A logged-in homepage is not enough. Instagram can accept the browser
    # session shell while rejecting the media-info endpoint used by the real
    # authenticated fallback. Probe that production path before declaring a
    # recovered cookie healthy, otherwise the recovery UI produces a false
    # success and the first real request immediately quarantines the account.
    if not POST_ID_RE.match(COOKIE_VALIDATION_POST_ID):
        raise HelperError("invalid_config", "cookie validation post ID is invalid")
    try:
        probe_media_id = str(shortcode_to_media_id(COOKIE_VALIDATION_POST_ID))
        probe_referer = f"https://www.instagram.com/p/{COOKIE_VALIDATION_POST_ID}/"
        fetch_media_info(probe_media_id, account, probe_referer, cooldown_on_auth_failure=False)
    except Exception as exc:
        code = classify_exception(exc)
        if code in COOKIE_MEDIA_PROBE_ACCEPTED_RESTRICTIONS:
            # The media endpoint recognized the request but the fixed probe
            # publication itself is restricted. This still proves that the
            # session reached the production API without a login challenge.
            log("info", "cookie media API probe accepted content restriction", error_code=code)
        else:
            record_account_test(account, code, username=username, message="authenticated media API probe failed")
            if quarantine_on_failure:
                quarantine_account(account.account_id, code)
            raise HelperError(code, "authenticated media API probe failed") from exc
    if quarantine_on_failure:
        clear_account_quarantine(account.account_id)
    record_account_test(account, "ok", username=username)
    return {"slot": cookie_account_slot(account), "account_id": account.account_id, "username": username, "status": status, "code": "ok"}


def shortcode_to_media_id(shortcode):
    media_id = 0
    for ch in shortcode:
        media_id = media_id * 64 + SHORTCODE_ALPHABET.index(ch)
    return media_id


def valid_url(raw):
    parsed = urlparse(str(raw or ""))
    return parsed.scheme in ("http", "https") and bool(parsed.netloc)


def valid_video_proxy_url(raw):
    parsed = urlparse(str(raw or ""))
    host = (parsed.hostname or "").lower()
    return parsed.scheme == "https" and any(host == suffix[1:] or host.endswith(suffix) for suffix in VIDEO_PROXY_ALLOWED_HOST_SUFFIXES)


def positive_int(value):
    try:
        n = int(value)
        return n if n > 0 else 0
    except Exception:
        return 0


def video_sort_key(version):
    width = positive_int((version or {}).get("width"))
    height = positive_int((version or {}).get("height"))
    area = width * height if width and height else 0
    # Prefer versions where Instagram provides dimensions, then choose the
    # smallest resolution. Unknown-size URLs are kept as fallback candidates.
    return (0 if area else 1, area if area else 2**63 - 1)


def video_candidates(versions):
    candidates = []
    for version in versions or []:
        raw = str((version or {}).get("url") or "").strip()
        if not valid_url(raw):
            continue
        width, height = video_dimensions(version)
        candidates.append((video_sort_key(version), raw, width, height))
    return sorted(candidates, key=lambda item: item[0])


def video_dimensions(version):
    return positive_int((version or {}).get("width")), positive_int((version or {}).get("height"))


def verify_video_url(url, referer):
    if not VERIFY_VIDEO_URL:
        return True
    headers = {
        "accept": "video/mp4,video/*,*/*;q=0.8",
        "range": "bytes=0-0",
        "referer": referer,
    }
    try:
        r = requests.get(url, headers=headers, impersonate=IMPERSONATE, timeout=TIMEOUT, allow_redirects=False, stream=True)
    except Exception as exc:
        log("warn", "video url verification request failed", error=str(exc)[:200])
        return False
    try:
        content_type = (r.headers.get("content-type") or "").lower()
        if r.status_code not in (200, 206):
            log("warn", "video url verification rejected status", status=r.status_code, content_type=content_type[:80])
            return False
        if r.status_code == 200 and content_length_too_large(r.headers):
            log("warn", "video url verification rejected large response", content_length=r.headers.get("content-length"))
            return False
        if content_type and not ("video" in content_type or "octet-stream" in content_type):
            log("warn", "video url verification rejected content type", status=r.status_code, content_type=content_type[:80])
            return False
        return True
    finally:
        with contextlib.suppress(Exception):
            r.close()


def select_video_url(versions, referer):
    for _, raw, width, height in video_candidates(versions):
        if verify_video_url(raw, referer):
            return raw, width, height
    return "", 0, 0


def fetch_media_info(media_id, account, referer, cooldown_on_auth_failure=True):
    url = f"https://www.instagram.com/api/v1/media/{quote(str(media_id), safe='')}/info/"
    headers = {
        "accept": "application/json,text/html,*/*",
        "accept-language": "ru,en-US;q=0.9,en;q=0.8,uk;q=0.7",
        "cookie": account.cookie,
        "referer": referer,
        "x-ig-app-id": "936619743392459",
        "x-asbd-id": "129477",
        "x-requested-with": "XMLHttpRequest",
    }
    r = auth_get(url, headers, post_id=str(media_id), account=account, stream=True, mark_account_failures=cooldown_on_auth_failure)
    body = bounded_response_bytes(r)
    if r.status_code != 200:
        code = classify_instagram_error(r.status_code, body)
        if cooldown_on_auth_failure:
            mark_account_failure(account.account_id, code, str(media_id))
        raise HelperError(code, f"media info HTTP {r.status_code}", r.status_code)
    try:
        data = json.loads(body.decode("utf-8"))
    except json.JSONDecodeError as exc:
        raise HelperError("invalid_response", "media info returned invalid JSON") from exc
    items = data.get("items") or []
    if not items:
        code = classify_instagram_error(r.status_code, body, "empty_media_info")
        raise HelperError(code, "media info returned no items")
    return items[0]


def media_info_payload(post_id, media_id, account, referer):
    item = fetch_media_info(media_id, account, referer)
    username = str(((item.get("user") or {}).get("username")) or "instagram").strip().lstrip("@")
    caption = str(((item.get("caption") or {}).get("text")) or "").strip()

    thumbnail = ""
    candidates = ((item.get("image_versions2") or {}).get("candidates")) or []
    if candidates:
        thumbnail = str((candidates[0] or {}).get("url") or "").strip()
    if not valid_url(thumbnail):
        raise HelperError("invalid_response", "media info response has no valid thumbnail")

    video_url = ""
    width = 0
    height = 0
    versions = item.get("video_versions") or []
    if versions:
        video_url, width, height = select_video_url(versions, referer)

    return {
        "ok": True,
        "post_id": post_id,
        "username": username,
        "caption": caption,
        "thumbnail_url": thumbnail,
        "video_url": video_url,
        "width": width,
        "height": height,
        "media_id": str(item.get("id") or media_id),
    }


def media_info_video_url(media_id, account, referer):
    item = fetch_media_info(media_id, account, referer, cooldown_on_auth_failure=False)
    versions = item.get("video_versions") or []
    if not versions:
        return "", 0, 0
    return select_video_url(versions, referer)


def content_length_too_large(headers):
    try:
        length = int(headers.get("content-length") or "0")
    except Exception:
        length = 0
    return length > VIDEO_PROXY_MAX_BYTES


def header_int(headers, name):
    try:
        return int(headers.get(name) or "0")
    except Exception:
        return 0


def parse_content_range(value):
    match = CONTENT_RANGE_RE.match(str(value or "").strip())
    if not match:
        return None
    start = int(match.group(1))
    end = int(match.group(2))
    total = 0 if match.group(3) == "*" else int(match.group(3))
    if end < start:
        return None
    return start, end, total


def response_transfer_plan(response):
    content_range = parse_content_range(response.headers.get("content-range"))
    if response.status_code == 206 and content_range:
        start, end, total = content_range
        return end - start + 1, start, end, total
    content_length = header_int(response.headers, "content-length")
    if response.status_code == 200 and content_length > 0:
        return content_length, 0, content_length - 1, content_length
    return 0, 0, None, 0


def parse_request_range(value):
    match = REQUEST_RANGE_RE.match(str(value or "").strip())
    if not match:
        return None
    start = int(match.group(1))
    end = int(match.group(2)) if match.group(2) else None
    if end is not None and end < start:
        return None
    return start, end


def chunk_end(start, final_end):
    size = max(1, VIDEO_PROXY_UPSTREAM_CHUNK_BYTES)
    end = start + size - 1
    if final_end is not None:
        end = min(end, final_end)
    return end


def chunk_range_header(start, final_end):
    return f"bytes={start}-{chunk_end(start, final_end)}"


def unique_urls(urls):
    seen = set()
    unique = []
    for raw in urls:
        raw = str(raw or "").strip()
        if not raw or raw in seen:
            continue
        seen.add(raw)
        unique.append(raw)
    return unique


def refreshed_video_urls(post_id, account, referer, current_url=""):
    try:
        item = fetch_media_info(str(shortcode_to_media_id(post_id)), account, referer, cooldown_on_auth_failure=False)
    except Exception as exc:
        log("warn", "video proxy refresh lookup failed", post_id=post_id, error=str(exc)[:300])
        return []
    urls = []
    for _, raw, _, _ in video_candidates(item.get("video_versions") or []):
        if valid_video_proxy_url(raw):
            urls.append(raw)
    urls = unique_urls(urls)
    if urls and (not current_url or urls[0] != current_url):
        log("info", "video proxy refreshed video url", post_id=post_id)
    return urls


def video_refresh_enabled():
    return VIDEO_PROXY_REFRESH_MODE not in ("0", "false", "off", "never", "disabled", "none")


def maybe_refreshed_video_urls(post_id, referer, current_url=""):
    if not video_refresh_enabled():
        return []
    remaining, reason = auth_circuit_status()
    if remaining > 0:
        log("warn", "video proxy auth refresh skipped: circuit open", post_id=post_id, reason=reason, remaining_seconds=remaining)
        return []
    try:
        account = choose_cookie_account(post_id)
    except Exception as exc:
        log("warn", "video proxy auth refresh skipped: cookie unavailable", post_id=post_id, error=str(exc)[:160])
        return []
    return refreshed_video_urls(post_id, account, referer, current_url)


def response_body_prefix(response, limit=1024):
    with contextlib.suppress(Exception):
        return bounded_stream_prefix(response, limit)
    return b""


def open_video_response(url, headers, timeout=None):
    response = requests.get(url, headers=headers, impersonate=IMPERSONATE, timeout=timeout or VIDEO_PROXY_TIMEOUT, allow_redirects=False, stream=True)
    if response.status_code in REDIRECT_STATUSES:
        location = response.headers.get("location", "")
        code = classify_redirect_location(location)
        with contextlib.suppress(Exception):
            response.close()
        raise HelperError(code, f"video upstream redirected to {location[:160] or 'unknown'}", response.status_code)
    return response


def open_initial_chunk_response(url, headers, requested_range):
    chunk_headers = dict(headers)
    if requested_range:
        start, final_end = requested_range
    else:
        start, final_end = 0, None
    chunk_headers["range"] = chunk_range_header(start, final_end)
    return open_video_response(url, chunk_headers, timeout=VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT)


def acceptable_video_response(response):
    content_type = (response.headers.get("content-type") or "").lower()
    return response.status_code in (200, 206) and (not content_type or "video" in content_type or "octet-stream" in content_type)


def resume_range_header(next_absolute_byte, end_absolute_byte):
    if end_absolute_byte is None:
        return f"bytes={next_absolute_byte}-"
    return f"bytes={next_absolute_byte}-{end_absolute_byte}"


def open_resume_response(post_id, url, headers, next_absolute_byte, end_absolute_byte, expected_total):
    resume_headers = dict(headers)
    resume_headers["range"] = chunk_range_header(next_absolute_byte, end_absolute_byte)
    response = open_video_response(url, resume_headers, timeout=VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT)
    content_type = (response.headers.get("content-type") or "").lower()
    content_range = parse_content_range(response.headers.get("content-range"))
    if response.status_code != 206 or not content_range or content_range[0] != next_absolute_byte:
        error_code = classify_instagram_error(response.status_code, response_body_prefix(response), "video_proxy_resume_rejected")
        with contextlib.suppress(Exception):
            response.close()
        log("warn", "video proxy resume rejected", post_id=post_id, status=response.status_code, content_type=content_type[:80], error_code=error_code)
        return None
    if expected_total > 0 and content_range[2] > 0 and content_range[2] != expected_total:
        with contextlib.suppress(Exception):
            response.close()
        log("warn", "video proxy resume rejected total mismatch", post_id=post_id, expected_total=expected_total, got_total=content_range[2])
        return None
    if content_type and not ("video" in content_type or "octet-stream" in content_type):
        with contextlib.suppress(Exception):
            response.close()
        log("warn", "video proxy resume rejected content type", post_id=post_id, status=response.status_code, content_type=content_type[:80])
        return None
    return response


def proxy_video(handler, post_id, video_url, head_only=False):
    if not ENABLE_VIDEO_PROXY:
        handler.write_json(404, {"ok": False, "error": "video proxy disabled"})
        return
    if not valid_video_proxy_url(video_url):
        handler.write_json(400, {"ok": False, "error": "invalid video url"})
        return
    if not video_proxy_semaphore.acquire(blocking=False):
        handler.write_json(503, {"ok": False, "error": "video proxy busy"})
        log("warn", "video proxy concurrency limit", post_id=post_id)
        return

    start = time.time()
    sent = 0
    response = None
    headers_sent = False
    try:
        referer = f"https://www.instagram.com/reel/{post_id}/"
        headers = {
            "accept": "video/mp4,video/*,*/*;q=0.8",
            "accept-language": "ru,en-US;q=0.9,en;q=0.8,uk;q=0.7",
            "referer": referer,
        }
        if VIDEO_PROXY_SEND_COOKIE:
            try:
                headers["cookie"] = choose_cookie_account(post_id).cookie
            except Exception as exc:
                log("warn", "video proxy cookie unavailable; continuing without cookie", post_id=post_id, error=str(exc)[:160])
        range_header = handler.headers.get("Range")
        requested_range = parse_request_range(range_header)

        candidate_urls = unique_urls([video_url])
        current_url = ""
        last_status = 0
        last_content_type = ""
        last_error_code = "video_proxy_upstream_rejected"
        refreshed_once = False
        while True:
            for candidate_url in candidate_urls:
                try:
                    response = open_initial_chunk_response(candidate_url, headers, requested_range)
                except Exception as exc:
                    last_error_code = classify_exception(exc)
                    log("warn", "video proxy upstream request failed", post_id=post_id, error=str(exc)[:300], error_code=last_error_code)
                    response = None
                    continue
                content_type = (response.headers.get("content-type") or "").lower()
                if acceptable_video_response(response):
                    current_url = candidate_url
                    break
                last_status = response.status_code
                last_content_type = content_type
                last_error_code = classify_instagram_error(response.status_code, response_body_prefix(response), "video_proxy_upstream_rejected")
                with contextlib.suppress(Exception):
                    response.close()
                response = None
            if response is not None:
                break
            if refreshed_once or not video_refresh_enabled():
                break
            refreshed_once = True
            candidate_urls = unique_urls(maybe_refreshed_video_urls(post_id, referer, current_url))
            if not candidate_urls:
                break
            log("info", "video proxy retrying with refreshed candidates after upstream failure", post_id=post_id, candidates=len(candidate_urls))
        if response is None:
            handler.write_json(502, {"ok": False, "error": f"upstream video HTTP {last_status}", "error_code": last_error_code})
            log("warn", "video proxy upstream rejected", post_id=post_id, status=last_status, content_type=last_content_type[:80], error_code=last_error_code)
            return
        if content_length_too_large(response.headers):
            with contextlib.suppress(Exception):
                response.close()
            handler.write_json(413, {"ok": False, "error": "video too large"})
            log("warn", "video proxy rejected large response", post_id=post_id, content_length=response.headers.get("content-length"))
            return

        first_content_range = parse_content_range(response.headers.get("content-range"))
        if not first_content_range:
            with contextlib.suppress(Exception):
                response.close()
            handler.write_json(502, {"ok": False, "error": "upstream did not honor range request", "error_code": "video_proxy_range_rejected"})
            log("warn", "video proxy initial range rejected", post_id=post_id, status=response.status_code, content_range=response.headers.get("content-range"))
            return
        first_start, first_end, total_bytes = first_content_range
        if requested_range:
            requested_start, requested_end = requested_range
            if first_start != requested_start:
                with contextlib.suppress(Exception):
                    response.close()
                handler.write_json(502, {"ok": False, "error": "upstream returned wrong range", "error_code": "video_proxy_range_mismatch"})
                log("warn", "video proxy initial range mismatch", post_id=post_id, expected_start=requested_start, got_start=first_start)
                return
            start_byte = requested_start
            end_byte = requested_end if requested_end is not None else (total_bytes - 1 if total_bytes else None)
            expected_bytes = (end_byte - start_byte + 1) if end_byte is not None else 0
            downstream_status = 206
            downstream_content_range = f"bytes {start_byte}-{end_byte}/{total_bytes}" if end_byte is not None and total_bytes else response.headers.get("Content-Range")
            downstream_content_length = str(expected_bytes) if expected_bytes > 0 else ""
        else:
            start_byte = 0
            end_byte = total_bytes - 1 if total_bytes else None
            expected_bytes = total_bytes if total_bytes else 0
            downstream_status = 200
            downstream_content_range = ""
            downstream_content_length = str(total_bytes) if total_bytes else ""
        if expected_bytes > VIDEO_PROXY_MAX_BYTES:
            with contextlib.suppress(Exception):
                response.close()
            handler.write_json(413, {"ok": False, "error": "video too large"})
            log("warn", "video proxy rejected large response", post_id=post_id, expected=expected_bytes)
            return

        handler.send_response(downstream_status)
        for key in ("Content-Type", "Accept-Ranges", "Last-Modified", "ETag"):
            value = response.headers.get(key)
            if value:
                handler.send_header(key, value)
        if downstream_content_length:
            handler.send_header("Content-Length", downstream_content_length)
        if downstream_content_range:
            handler.send_header("Content-Range", downstream_content_range)
        handler.send_header("Cache-Control", "public, max-age=300")
        handler.end_headers()
        headers_sent = True

        resume_attempts = 0
        complete = True
        abort_stream = False
        if not head_only:
            while True:
                try:
                    for chunk in response.iter_content(chunk_size=64 * 1024):
                        if not chunk:
                            continue
                        if sent + len(chunk) > VIDEO_PROXY_MAX_BYTES:
                            complete = False
                            abort_stream = True
                            log("warn", "video proxy byte limit reached", post_id=post_id, sent=sent + len(chunk))
                            break
                        handler.wfile.write(chunk)
                        sent += len(chunk)
                    if abort_stream:
                        break
                    if expected_bytes <= 0 or sent >= expected_bytes:
                        break
                    if sent >= VIDEO_PROXY_MAX_BYTES:
                        complete = False
                        break
                    log("warn", "video proxy upstream short read", post_id=post_id, sent=sent, expected=expected_bytes)
                except (BrokenPipeError, ConnectionResetError):
                    raise
                except Exception as exc:
                    if expected_bytes <= 0:
                        complete = False
                        log("warn", "video proxy stream interrupted", post_id=post_id, error=str(exc)[:300], sent=sent)
                        break
                    log("warn", "video proxy stream interrupted; attempting resume", post_id=post_id, error=str(exc)[:300], sent=sent, expected=expected_bytes)

                if expected_bytes <= 0 or sent >= expected_bytes:
                    break
                if resume_attempts >= max(0, VIDEO_PROXY_MAX_RESUME_ATTEMPTS):
                    complete = False
                    log("warn", "video proxy resume attempts exhausted", post_id=post_id, sent=sent, expected=expected_bytes, attempts=resume_attempts)
                    break

                next_byte = start_byte + sent
                if end_byte is not None and next_byte > end_byte:
                    break
                with contextlib.suppress(Exception):
                    response.close()
                resume_attempts += 1
                try:
                    resumed = open_resume_response(post_id, current_url, headers, next_byte, end_byte, total_bytes)
                except Exception as exc:
                    resumed = None
                    log("warn", "video proxy resume request failed", post_id=post_id, error=str(exc)[:300], attempts=resume_attempts)
                if resumed is None:
                    for fresh_url in maybe_refreshed_video_urls(post_id, referer, current_url):
                        try:
                            resumed = open_resume_response(post_id, fresh_url, headers, next_byte, end_byte, total_bytes)
                        except Exception as exc:
                            resumed = None
                            log("warn", "video proxy resume request failed", post_id=post_id, error=str(exc)[:300], attempts=resume_attempts)
                        if resumed is not None:
                            current_url = fresh_url
                            break
                if resumed is None:
                    complete = False
                    log("warn", "video proxy unable to resume stream", post_id=post_id, sent=sent, expected=expected_bytes, attempts=resume_attempts)
                    break
                response = resumed
                log("info", "video proxy resumed stream", post_id=post_id, next_byte=next_byte, attempts=resume_attempts)
        level = "info" if complete else "warn"
        log(level, "video proxy succeeded" if complete else "video proxy incomplete", post_id=post_id, status=response.status_code, sent=sent, expected=expected_bytes, resumes=resume_attempts, duration_ms=int((time.time() - start) * 1000))
    except (BrokenPipeError, ConnectionResetError):
        log("warn", "video proxy client disconnected", post_id=post_id, sent=sent)
    except Exception as exc:
        if sent == 0 and not headers_sent:
            handler.write_json(502, {"ok": False, "error": str(exc)[:300], "error_code": classify_exception(exc)})
        log("warn", "video proxy failed", post_id=post_id, error=str(exc)[:300], sent=sent)
    finally:
        if response is not None:
            with contextlib.suppress(Exception):
                response.close()
        video_proxy_semaphore.release()


class Handler(BaseHTTPRequestHandler):
    server_version = "InstaFixAuthHelper/1.0"

    def log_message(self, fmt, *args):
        return

    def write_json(self, code, payload):
        raw = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def read_json(self, limit=65536):
        length = int(self.headers.get("Content-Length") or "0")
        if length <= 0 or length > limit:
            raise HelperError("invalid_request", "invalid request body size")
        raw = self.rfile.read(length)
        return json.loads(raw.decode("utf-8"))

    def admin_authorized(self):
        if not COOKIE_ADMIN_TOKEN:
            return False
        return hmac.compare_digest(self.headers.get("X-Admin-Token", ""), COOKIE_ADMIN_TOKEN)

    def do_GET(self):
        try:
            if self.path == "/healthz":
                pool = cookie_pool_health()
                has_cookie = pool["total"] > 0
                remaining, reason = auth_circuit_status()
                self.write_json(200 if has_cookie else 503, {"ok": has_cookie, "cookie_pool": pool, "auth_circuit_open": remaining > 0, "auth_circuit_remaining_seconds": remaining, "auth_circuit_reason": reason})
                return
            if self.path == "/accounts":
                self.write_json(200, {"ok": True, "accounts": cookie_accounts_status()})
                return
            video_prefix = "/video/"
            if self.path.startswith(video_prefix):
                self.handle_video_proxy(False)
                return
            prefix = "/oembed/"
            if not self.path.startswith(prefix):
                self.write_json(404, {"ok": False, "error": "not found"})
                return
            raw_post, _, raw_query = self.path[len(prefix):].partition("?")
            post_id = raw_post.strip()
            if not POST_ID_RE.match(post_id):
                self.write_json(400, {"ok": False, "error": "invalid post_id"})
                return
            query = parse_qs(raw_query)
            account_ref = (query.get("account") or [""])[0]
            bypass_cache = (query.get("bypass_cache") or [""])[0].strip().lower() in ("1", "true", "yes", "on")
            forced_account = None
            if account_ref:
                forced_account = find_cookie_account(account_ref)
                if forced_account is None:
                    self.write_json(404, {"ok": False, "error": "cookie account not found", "error_code": "account_not_found"})
                    return
            if not allow_request():
                self.write_json(429, {"ok": False, "error": "rate limited"})
                log("warn", "auth helper rate limited", post_id=post_id)
                return
            start = time.time()
            data = oembed(post_id, forced_account=forced_account, bypass_cache=bypass_cache)
            if forced_account is not None:
                data = dict(data)
                data["account_id"] = forced_account.account_id
                data["slot"] = cookie_account_slot(forced_account)
            self.write_json(200, data)
            log("info", "auth oembed succeeded", post_id=post_id, duration_ms=int((time.time() - start) * 1000))
        except Exception as exc:
            code = classify_exception(exc)
            self.write_json(502, {
                "ok": False,
                "error": str(exc)[:300],
                "error_code": code,
                "cache_hit": bool(getattr(exc, "cached", False)),
            })
            post_id = ""
            if self.path.startswith("/oembed/"):
                post_id = self.path[len("/oembed/"):].split("?", 1)[0].strip()
            log("warn", "auth oembed failed", post_id=post_id, error=str(exc)[:300], error_code=code)

    def do_POST(self):
        try:
            if self.path == "/admin/cookies/test":
                if not self.admin_authorized():
                    self.write_json(403, {"ok": False, "error": "forbidden"})
                    return
                payload = self.read_json(8192)
                slot = str(payload.get("slot") or "").strip()
                result = test_cookie_homepage(slot)
                self.write_json(200, {"ok": True, "result": result, "accounts": cookie_accounts_status()})
                log("info", "cookie recovery test succeeded", slot=result.get("slot", slot), account_id=result.get("account_id", ""), username=result.get("username", ""))
                return
            if self.path != "/admin/cookies/update":
                self.write_json(404, {"ok": False, "error": "not found"})
                return
            if not self.admin_authorized():
                self.write_json(403, {"ok": False, "error": "forbidden"})
                return
            payload = self.read_json(min(COOKIE_ADMIN_MAX_BYTES + 4096, 131072))
            slot = str(payload.get("slot") or "").strip()
            cookie = str(payload.get("cookie") or "")
            expected_username = str(payload.get("expected_username") or "").strip()
            result = update_cookie_slot(slot, cookie, expected_username)
            self.write_json(200, {"ok": True, "result": result, "accounts": cookie_accounts_status()})
            log("info", "cookie slot updated", slot=result.get("slot", slot), account_id=result.get("account_id", ""), file=result.get("file", ""), has_sessionid=True)
        except Exception as exc:
            code = classify_exception(exc)
            self.write_json(400 if code in ("invalid_slot", "cookie_missing", "cookie_too_large", "invalid_request", "account_mismatch") else 502, {"ok": False, "error": str(exc)[:300], "error_code": code})
            if self.path == "/admin/cookies/test":
                log("warn", "cookie recovery test failed", error=str(exc)[:160], error_code=code)
            else:
                log("warn", "cookie slot update failed", error=str(exc)[:160], error_code=code)

    def do_HEAD(self):
        if self.path.startswith("/video/"):
            self.handle_video_proxy(True)
            return
        self.write_json(404, {"ok": False, "error": "not found"})

    def handle_video_proxy(self, head_only):
        prefix = "/video/"
        raw_path, _, raw_query = self.path.partition("?")
        post_id = raw_path[len(prefix):].strip()
        if not POST_ID_RE.match(post_id):
            self.write_json(400, {"ok": False, "error": "invalid post_id"})
            return
        qs = parse_qs(raw_query, keep_blank_values=False)
        video_url = (qs.get("url") or [""])[0].strip()
        if not allow_request():
            self.write_json(429, {"ok": False, "error": "rate limited"})
            log("warn", "auth helper rate limited", post_id=post_id, route="video")
            return
        proxy_video(self, post_id, video_url, head_only=head_only)


def main():
    host, port_text = LISTEN.rsplit(":", 1)
    try:
        pool = cookie_pool_health()
        if pool["total"] <= 0:
            raise HelperError("cookie_missing", "no usable Instagram cookie files found")
    except Exception as exc:
        log("error", "auth helper cookie unavailable", error=str(exc))
        sys.exit(1)
    server = ThreadingHTTPServer((host, int(port_text)), Handler)
    log("info", "auth helper listening", listen=LISTEN, impersonate=IMPERSONATE, cookie_file=bool(COOKIE_FILE), cookie_dir=bool(COOKIE_DIR), cookie_pool=pool, cookie_admin=bool(COOKIE_ADMIN_TOKEN), cookie_validation_post=bool(COOKIE_VALIDATION_POST_ID), max_per_minute=MAX_PER_MINUTE, auth_max_per_minute=AUTH_MAX_PER_MINUTE, auth_cooldown_seconds=AUTH_COOLDOWN_SECONDS, auth_cache_ttl_seconds=AUTH_CACHE_TTL_SECONDS, auth_negative_cache_ttl_seconds=AUTH_NEGATIVE_CACHE_TTL_SECONDS, fetch_video_info=FETCH_VIDEO_INFO, fetch_media_info_fallback=FETCH_MEDIA_INFO_FALLBACK, video_proxy=ENABLE_VIDEO_PROXY, video_proxy_send_cookie=VIDEO_PROXY_SEND_COOKIE, video_proxy_refresh_mode=VIDEO_PROXY_REFRESH_MODE, video_proxy_max_concurrent=VIDEO_PROXY_MAX_CONCURRENT, video_proxy_max_bytes=VIDEO_PROXY_MAX_BYTES, video_proxy_timeout=VIDEO_PROXY_TIMEOUT, video_proxy_max_resume_attempts=VIDEO_PROXY_MAX_RESUME_ATTEMPTS, video_proxy_upstream_chunk_bytes=VIDEO_PROXY_UPSTREAM_CHUNK_BYTES, video_proxy_upstream_chunk_timeout=VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT)
    server.serve_forever()


if __name__ == "__main__":
    main()
