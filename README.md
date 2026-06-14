# InstaFix Revived

> Instagram is a trademark of Instagram, Inc. This project is independent and is not affiliated with Instagram, Meta, or Instagram, Inc.

InstaFix Revived is a maintained, lightweight continuation of [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix). It serves cleaner OpenGraph/Twitter-card previews for public Instagram posts and Reels, with playable media links when Instagram exposes usable media URLs.

## Features

- Public-first Instagram metadata scraping.
- Rich preview pages for posts, Reels, stories-style paths, images, and videos.
- Optional local authenticated fallback helper using `curl_cffi` for restricted content.
- Optional selective preview-client video proxy with strict limits, disabled by default.
- Minimal JSON homepage preview endpoint that does not expose Instagram CDN URLs.
- Structured JSON logs to stdout.

## Quick start

```sh
go build
./instafix -listen 127.0.0.1:3000
```

Then open `http://127.0.0.1:3000/`.

To use it publicly, deploy behind a reverse proxy and replace `instagram.com` in a post/Reel URL with your own domain.

## Docker

```sh
docker build -t instafix-revived:local .
docker run --rm -p 3000:3000 instafix-revived:local
```

See `docker-compose.example.yml` for an app + optional auth-helper example.

## Optional auth helper

The auth helper can use an Instagram `Cookie` header to recover metadata for content that public scraping cannot access. Keep it loopback-only and mount cookies from files outside Git.

```sh
docker build -t instafix-auth-helper:local auth-helper
docker run --rm --network host \
  -v /opt/instafix-revived/secrets/instagram_cookie:/run/secrets/instagram_cookie:ro \
  -e AUTH_HELPER_LISTEN=127.0.0.1:3200 \
  instafix-auth-helper:local
```

Start the app with:

```sh
AUTH_HELPER_URL=http://127.0.0.1:3200 ./instafix
```

`AUTH_HELPER_URL` is intentionally restricted to `http://localhost` / loopback addresses.

## Safety notes

- Do not commit cookies, `.env` files, tokens, production configs, or logs.
- Do not expose the auth helper to the public internet.
- Use conservative rate limits and a dedicated account if you enable authenticated fallback.
- The video proxy is disabled by default; if enabled, restrict it to known preview clients and low concurrency.

## Attribution

Maintained by [Bl0ck154](https://github.com/Bl0ck154). Derived from and inspired by [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix).
