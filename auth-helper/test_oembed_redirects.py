import unittest
from unittest import mock

import app


class SafeOembedRedirectsTest(unittest.TestCase):
    def test_same_origin_oembed_redirect_rejects_external_cookie_destinations(self):
        origin = "https://www.instagram.com/api/v1/oembed/?url=post"
        allowed = [
            "/api/v1/oembed/?page=2",
            "https://www.instagram.com/api/v1/oembed/next",
        ]
        for location in allowed:
            self.assertTrue(app.same_origin_oembed_redirect(origin, location), location)
        denied = [
            "https://evil.example/api/v1/oembed/",
            "//evil.example/api/v1/oembed/",
            "http://www.instagram.com/api/v1/oembed/",
            "https://www.instagram.com.evil.example/api/v1/oembed/",
            "https://attacker@www.instagram.com/api/v1/oembed/",
            "https://www.instagram.com:8443/api/v1/oembed/",
            "https://www.instagram.com/accounts/login/",
            "/api/v1/feed/reels_media/",
        ]
        for location in denied:
            self.assertIsNone(app.same_origin_oembed_redirect(origin, location), location)

    def test_opt_in_follows_only_same_origin_oembed_and_counts_rate_budget(self):
        first = mock.Mock(status_code=302, headers={"location": "/api/v1/oembed/redirected"}, close=mock.Mock())
        second = mock.Mock(status_code=200, headers={}, close=mock.Mock())
        original = app.SAFE_OEMBED_REDIRECTS
        app.SAFE_OEMBED_REDIRECTS = True
        try:
            with mock.patch("app.require_auth_available"), mock.patch("app.allow_auth_request", return_value=True) as limiter:
                with mock.patch("app.requests.get", side_effect=[first, second]) as get:
                    headers = {"cookie": "sessionid=secret"}
                    response = app.auth_get(
                        "https://www.instagram.com/api/v1/oembed/?url=post",
                        headers,
                    )
                    self.assertIs(response, second)
                    self.assertEqual(get.call_count, 2)
                    self.assertEqual(limiter.call_count, 2)
                    self.assertEqual(
                        get.call_args.args[0],
                        "https://www.instagram.com/api/v1/oembed/redirected",
                    )
                    self.assertEqual(get.call_args.kwargs["headers"], headers)
        finally:
            app.SAFE_OEMBED_REDIRECTS = original

    def test_external_redirect_never_receives_cookie(self):
        first = mock.Mock(status_code=302, headers={"location": "https://evil.example/api/v1/oembed/"}, close=mock.Mock())
        original = app.SAFE_OEMBED_REDIRECTS
        app.SAFE_OEMBED_REDIRECTS = True
        try:
            with mock.patch("app.require_auth_available"), mock.patch("app.allow_auth_request", return_value=True):
                with mock.patch("app.requests.get", return_value=first) as get:
                    with self.assertRaises(app.HelperError) as caught:
                        app.auth_get(
                            "https://www.instagram.com/api/v1/oembed/?url=post",
                            {"cookie": "sessionid=secret"},
                        )
                    self.assertEqual(caught.exception.code, "redirected")
                    get.assert_called_once()
        finally:
            app.SAFE_OEMBED_REDIRECTS = original

    def test_redirect_following_disabled_by_default(self):
        first = mock.Mock(status_code=302, headers={"location": "/api/v1/oembed/redirected"}, close=mock.Mock())
        original = app.SAFE_OEMBED_REDIRECTS
        app.SAFE_OEMBED_REDIRECTS = False
        try:
            with mock.patch("app.require_auth_available"), mock.patch("app.allow_auth_request", return_value=True):
                with mock.patch("app.requests.get", return_value=first) as get:
                    with self.assertRaises(app.HelperError):
                        app.auth_get("https://www.instagram.com/api/v1/oembed/?url=post", {"cookie": "sessionid=secret"})
                    get.assert_called_once()
        finally:
            app.SAFE_OEMBED_REDIRECTS = original


if __name__ == "__main__":
    unittest.main()
