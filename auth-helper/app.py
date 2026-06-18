import contextlib
import glob
import hashlib
import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, quote, urlparse

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
POST_ID_RE = re.compile(r"^[A-Za-z0-9_-]{6,32}$")
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
cookie_pool_lock = threading.Lock()
cookie_pool = []
cookie_pool_loaded_at = 0.0
cookie_cursor = 0
account_cooldowns = {}
video_proxy_semaphore = threading.BoundedSemaphore(max(1, VIDEO_PROXY_MAX_CONCURRENT))

AUTH_CIRCUIT_CODES = {"login_required", "checkpoint_required", "challenge_required", "cookie_missing"}
REDIRECT_STATUSES = {301, 302, 303, 307, 308}


class HelperError(Exception):
    def __init__(self, code, message, status=None):
        super().__init__(message)
        self.code = code
        self.status = status


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
        items.append({
            "account_id": account.account_id,
            "slot": cookie_account_slot(account),
            "file": os.path.basename(account.source or ""),
            "available": cooldown <= 0,
            "cooldown_remaining_seconds": cooldown,
        })
    return items


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
    log("warn", "cookie account cooldown opened", account_id=account_id, post_id=post_id, error_code=code, cooldown_seconds=max(60, AUTH_COOLDOWN_SECONDS))


def choose_cookie_account(post_id=""):
    global cookie_cursor
    accounts = discover_cookie_accounts()
    if not accounts:
        raise HelperError("cookie_missing", "no usable Instagram cookie files found")
    now = time.time()
    with cookie_pool_lock:
        total = len(accounts)
        for _ in range(total):
            idx = cookie_cursor % total
            cookie_cursor += 1
            account = accounts[idx]
            if account_cooldowns.get(account.account_id, 0) <= now:
                return account
        soonest = min(account_cooldowns.get(account.account_id, 0) for account in accounts)
    remaining = int(max(1, soonest - now))
    raise HelperError("auth_circuit_open", f"all Instagram cookie accounts are cooling down for {remaining}s")


def cookie_pool_health():
    accounts = discover_cookie_accounts()
    healthy = 0
    cooling = 0
    for account in accounts:
        if cookie_account_status(account.account_id) > 0:
            cooling += 1
        else:
            healthy += 1
    return {"total": len(accounts), "available": healthy, "cooling_down": cooling}


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


def auth_get(url, headers, post_id="", account=None, timeout=None, stream=False, mark_account_failures=True):
    require_auth_available(post_id)
    if not allow_auth_request():
        raise HelperError("auth_rate_limited", "authenticated request rate limited", 429)
    response = requests.get(url, headers=headers, impersonate=IMPERSONATE, timeout=timeout or TIMEOUT, allow_redirects=False, stream=stream)
    if response.status_code in REDIRECT_STATUSES:
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
        raise HelperError(negative.get("code") or "auth_negative_cached", negative.get("message") or "cached authenticated failure")
    cached = cache_get(auth_success_cache, post_id)
    if cached:
        log("info", "auth oembed served from cache", post_id=post_id)
        return dict(cached)
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
        cache_negative(post_id, code, exc, account_id)
        raise
    try:
        body = bounded_response_bytes(r)
    except HelperError as exc:
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
        log("warn", "auth oembed unavailable; trying media info fallback", post_id=post_id, status=r.status_code, error=message[:120])
        try:
            payload = media_info_payload(post_id, str(shortcode_to_media_id(post_id)), account, post_url)
            store_auth_payload(post_id, payload)
            return payload
        except HelperError as exc:
            if exc.code == "private_media":
                code = classify_instagram_error(r.status_code, body, "auth_forbidden")
                cache_negative(post_id, code, f"Instagram oEmbed HTTP {r.status_code}: {message}", account_id)
                raise HelperError(code, f"Instagram oEmbed HTTP {r.status_code}: {message}", r.status_code)
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
    store_auth_payload(post_id, payload)
    return payload


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
            self.write_json(502, {"ok": False, "error": str(exc)[:300], "error_code": code})
            post_id = ""
            if self.path.startswith("/oembed/"):
                post_id = self.path[len("/oembed/"):].split("?", 1)[0].strip()
            log("warn", "auth oembed failed", post_id=post_id, error=str(exc)[:300], error_code=code)

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
    log("info", "auth helper listening", listen=LISTEN, impersonate=IMPERSONATE, cookie_file=bool(COOKIE_FILE), cookie_dir=bool(COOKIE_DIR), cookie_pool=pool, max_per_minute=MAX_PER_MINUTE, auth_max_per_minute=AUTH_MAX_PER_MINUTE, auth_cooldown_seconds=AUTH_COOLDOWN_SECONDS, auth_cache_ttl_seconds=AUTH_CACHE_TTL_SECONDS, auth_negative_cache_ttl_seconds=AUTH_NEGATIVE_CACHE_TTL_SECONDS, fetch_video_info=FETCH_VIDEO_INFO, video_proxy=ENABLE_VIDEO_PROXY, video_proxy_send_cookie=VIDEO_PROXY_SEND_COOKIE, video_proxy_refresh_mode=VIDEO_PROXY_REFRESH_MODE, video_proxy_max_concurrent=VIDEO_PROXY_MAX_CONCURRENT, video_proxy_max_bytes=VIDEO_PROXY_MAX_BYTES, video_proxy_timeout=VIDEO_PROXY_TIMEOUT, video_proxy_max_resume_attempts=VIDEO_PROXY_MAX_RESUME_ATTEMPTS, video_proxy_upstream_chunk_bytes=VIDEO_PROXY_UPSTREAM_CHUNK_BYTES, video_proxy_upstream_chunk_timeout=VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT)
    server.serve_forever()


if __name__ == "__main__":
    main()
