# Testing and local deployment

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

Start the app with the helper enabled:

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
