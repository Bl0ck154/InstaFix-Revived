# Testing and local deployment

For the simplest stateless setup, use `docker compose -f docker-compose.example.yml up -d --build`. By default the app is reachable on `127.0.0.1:3000` and the optional authenticated helper is not started. Put an HTTPS reverse proxy in front of the app before exposing it publicly, and use a domain name that doesn't incorporate someone else's trademark.


## App container

```sh
docker build -t instafix-revived:test .
docker run -d --name instafix-revived-test \
  --restart unless-stopped \
  -p 127.0.0.1:3100:3000 \
  instafix-revived:test
```

View logs:

```sh
docker logs -f instafix-revived-test
```

## Optional authenticated fallback helper

The helper uses `curl_cffi` with a browser-like TLS fingerprint. It must remain local-only because it can read Instagram session cookies from mounted files.

Store cookies outside Git:

```sh
sudo install -d -m 700 /opt/instafix-revived/secrets
sudo sh -c 'printf %s "csrftoken=...; ds_user_id=...; sessionid=..." > /opt/instafix-revived/secrets/instagram_cookie'
sudo chmod 600 /opt/instafix-revived/secrets/instagram_cookie
```

Run the helper on loopback:

```sh
docker build -t instafix-auth-helper:test auth-helper
docker run -d --name instafix-auth-helper-test \
  --restart unless-stopped \
  --network host \
  -v /opt/instafix-revived/secrets/instagram_cookie:/run/secrets/instagram_cookie:ro \
  -e AUTH_HELPER_LISTEN=127.0.0.1:3200 \
  -e AUTH_HELPER_MAX_PER_MINUTE=20 \
  instafix-auth-helper:test
```

Start the app with the helper enabled (first remove the app container started in the earlier example, so Docker can reuse its name):

```sh
docker rm -f instafix-revived-test
```


```sh
docker run -d --name instafix-revived-test \
  --restart unless-stopped \
  --network host \
  -e AUTH_HELPER_URL=http://127.0.0.1:3200 \
  instafix-revived:test -listen 127.0.0.1:3100
```

Check helper health:

```sh
curl http://127.0.0.1:3200/healthz
curl http://127.0.0.1:3200/accounts
```

## Selective preview video proxy

If Instagram's CDN rejects a preview client, you can enable a limited streaming proxy. It is intentionally disabled by default, local-only, no-disk-cache, no-transcoding, and guarded by concurrency/size limits.

Helper-side flags:

```sh
-e AUTH_HELPER_ENABLE_VIDEO_PROXY=1 \
-e AUTH_HELPER_VIDEO_PROXY_SEND_COOKIE=0 \
-e AUTH_HELPER_VIDEO_PROXY_REFRESH_MODE=on_failure \
-e AUTH_HELPER_VIDEO_PROXY_MAX_CONCURRENT=1 \
-e AUTH_HELPER_VIDEO_PROXY_MAX_BYTES=50000000 \
-e AUTH_HELPER_VIDEO_PROXY_TIMEOUT_SECONDS=20 \
-e AUTH_HELPER_VIDEO_PROXY_UPSTREAM_CHUNK_BYTES=262144 \
-e AUTH_HELPER_VIDEO_PROXY_UPSTREAM_CHUNK_TIMEOUT_SECONDS=6
```

App-side flags:

```sh
-e PREVIEW_VIDEO_PROXY_ENABLED=1 \
-e PREVIEW_VIDEO_PROXY_USER_AGENTS='telegrambot,discordbot,whatsapp,slackbot' \
-e PREVIEW_VIDEO_PROXY_TIMEOUT_SECONDS=25
```

Do not set `PREVIEW_VIDEO_PROXY_USER_AGENTS=*` on a public service unless you intentionally want broad proxy behavior.

## Unit tests

```sh
python -m py_compile auth-helper/app.py auth-helper/test_app.py
python -m unittest discover -s auth-helper -p 'test_*.py'
```

Run `go test ./...` when Go is available.

## Telegram Reel regression checks

The recommended current runtime is the stateless path:

```sh
docker run --rm -p 127.0.0.1:3100:3000 \
  -e INSTAFIX_EXPERIMENT_MODE=stateless_cloudrun \
  -e INSTAFIX_PUBLIC_VIDEO_REFRESH_DIRECT=true \
  -e MAX_INLINE_VIDEO_BYTES=20971520 \
  instafix-revived:test
```

For Telegram Reel tests, verify both sides of the measured boundary:

- an original at or below `20,971,520` bytes should remain on the normal offload/direct path;
- an original above `20,971,520` bytes should advertise `?compact=av4`;
- a compact smart result must remain below the boundary (the encoder target is `20,471,520` bytes);
- diagnostics or cache-busting tests that need guaranteed fresh Telegram ingestion should use a unique media **path**, not only a changed query string.

The full investigation and August 2026 tested behavior are documented in [`docs/telegram-reel-previews.md`](docs/telegram-reel-previews.md).

Run the complete Go suite before publishing changes:

```sh
go test ./...
```

Also validate the Cloudflare Worker syntax when it changes:

```sh
node --check deploy/cloudflare-worker/src/index.js
```
