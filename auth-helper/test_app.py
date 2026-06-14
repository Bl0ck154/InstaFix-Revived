import unittest
from unittest import mock

import app


class FakeHeaders(dict):
    def get(self, key, default=None):
        return super().get(str(key).lower(), default)


class FakeResponse:
    def __init__(self, status_code, headers):
        self.status_code = status_code
        self.headers = FakeHeaders({str(k).lower(): str(v) for k, v in headers.items()})


class VideoProxyHelpersTest(unittest.TestCase):
    def test_parse_content_range(self):
        self.assertEqual(app.parse_content_range("bytes 10-19/100"), (10, 19, 100))
        self.assertEqual(app.parse_content_range("bytes 10-19/*"), (10, 19, 0))
        self.assertIsNone(app.parse_content_range("bytes 19-10/100"))
        self.assertIsNone(app.parse_content_range("items 10-19/100"))

    def test_resume_range_header(self):
        self.assertEqual(app.resume_range_header(524288, 1048575), "bytes=524288-1048575")
        self.assertEqual(app.resume_range_header(524288, None), "bytes=524288-")

    def test_parse_request_range(self):
        self.assertEqual(app.parse_request_range("bytes=10-19"), (10, 19))
        self.assertEqual(app.parse_request_range("bytes=10-"), (10, None))
        self.assertIsNone(app.parse_request_range("bytes=19-10"))
        self.assertIsNone(app.parse_request_range("items=10-19"))

    def test_chunk_range_header_caps_to_chunk_and_requested_end(self):
        old_size = app.VIDEO_PROXY_UPSTREAM_CHUNK_BYTES
        try:
            app.VIDEO_PROXY_UPSTREAM_CHUNK_BYTES = 512
            self.assertEqual(app.chunk_range_header(0, None), "bytes=0-511")
            self.assertEqual(app.chunk_range_header(512, 1023), "bytes=512-1023")
            self.assertEqual(app.chunk_range_header(512, 700), "bytes=512-700")
        finally:
            app.VIDEO_PROXY_UPSTREAM_CHUNK_BYTES = old_size

    def test_response_transfer_plan_for_partial_content(self):
        response = FakeResponse(206, {"content-range": "bytes 524288-1048575/6796967"})

        self.assertEqual(app.response_transfer_plan(response), (524288, 524288, 1048575, 6796967))

    def test_response_transfer_plan_for_full_content(self):
        response = FakeResponse(200, {"content-length": "1818524"})

        self.assertEqual(app.response_transfer_plan(response), (1818524, 0, 1818523, 1818524))

    def test_video_candidates_are_sorted_by_smallest_known_resolution(self):
        versions = [
            {"url": "https://scontent.cdninstagram.com/large.mp4", "width": 1080, "height": 1920},
            {"url": "https://scontent.cdninstagram.com/unknown.mp4"},
            {"url": "https://scontent.cdninstagram.com/small.mp4", "width": 720, "height": 1280},
            {"url": "not a url", "width": 1, "height": 1},
        ]

        urls = [candidate[1] for candidate in app.video_candidates(versions)]

        self.assertEqual(
            urls,
            [
                "https://scontent.cdninstagram.com/small.mp4",
                "https://scontent.cdninstagram.com/large.mp4",
                "https://scontent.cdninstagram.com/unknown.mp4",
            ],
        )

    def test_unique_urls_preserves_order(self):
        self.assertEqual(app.unique_urls(["a", "b", "a", "", "c", "b"]), ["a", "b", "c"])

    def test_classify_redirect_location(self):
        self.assertEqual(app.classify_redirect_location("https://www.instagram.com/accounts/login/"), "login_required")
        self.assertEqual(app.classify_redirect_location("https://www.instagram.com/challenge/abc"), "challenge_required")
        self.assertEqual(app.classify_redirect_location("https://www.instagram.com/checkpoint/abc"), "checkpoint_required")
        self.assertEqual(app.classify_redirect_location("https://www.instagram.com/other"), "redirected")

    def test_cache_helpers_expire_entries(self):
        cache = {}
        app.cache_set(cache, "a", {"ok": True}, 60)
        self.assertEqual(app.cache_get(cache, "a"), {"ok": True})
        app.cache_set(cache, "b", "expired", -1)
        self.assertIsNone(app.cache_get(cache, "b"))

    def test_auth_circuit_opens_for_login_required(self):
        old_until = app.auth_cooldown_until
        old_reason = app.auth_cooldown_reason
        try:
            app.auth_cooldown_until = 0
            app.auth_cooldown_reason = ""
            app.mark_auth_failure("login_required", "POSTID")
            remaining, reason = app.auth_circuit_status()
            self.assertGreater(remaining, 0)
            self.assertEqual(reason, "login_required")
        finally:
            app.auth_cooldown_until = old_until
            app.auth_cooldown_reason = old_reason

    def test_cookie_account_id_prefers_ds_user_id(self):
        self.assertEqual(app.cookie_account_id("csrftoken=a; ds_user_id=12345; sessionid=s"), "ig_12345")

    def test_choose_cookie_account_skips_cooling_account(self):
        a = app.CookieAccount("a", "sessionid=a; ds_user_id=a", "/tmp/a")
        b = app.CookieAccount("b", "sessionid=b; ds_user_id=b", "/tmp/b")
        old_cursor = app.cookie_cursor
        old_cooldowns = dict(app.account_cooldowns)
        try:
            app.cookie_cursor = 0
            app.account_cooldowns.clear()
            app.account_cooldowns["a"] = app.time.time() + 60
            with mock.patch("app.discover_cookie_accounts", return_value=[a, b]):
                self.assertEqual(app.choose_cookie_account().account_id, "b")
        finally:
            app.cookie_cursor = old_cursor
            app.account_cooldowns.clear()
            app.account_cooldowns.update(old_cooldowns)

    def test_cookie_pool_health_counts_cooldowns(self):
        a = app.CookieAccount("a", "sessionid=a; ds_user_id=a", "/tmp/a")
        b = app.CookieAccount("b", "sessionid=b; ds_user_id=b", "/tmp/b")
        old_cooldowns = dict(app.account_cooldowns)
        try:
            app.account_cooldowns.clear()
            app.account_cooldowns["a"] = app.time.time() + 60
            with mock.patch("app.discover_cookie_accounts", return_value=[a, b]):
                self.assertEqual(app.cookie_pool_health(), {"total": 2, "available": 1, "cooling_down": 1})
        finally:
            app.account_cooldowns.clear()
            app.account_cooldowns.update(old_cooldowns)


if __name__ == "__main__":
    unittest.main()
