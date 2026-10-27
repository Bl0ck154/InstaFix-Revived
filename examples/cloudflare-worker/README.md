# Optional Cloudflare Worker example

This directory contains a generic edge layer for self-hosted InstaFix deployments. It is optional: InstaFix can run directly behind Caddy, Nginx, another reverse proxy, or a different CDN.

## What the Worker adds

- Proxies public application requests to your private/public InstaFix origin.
- Rewrites origin URLs in returned HTML back to the public edge hostname.
- Preserves normal Instagram CDN redirects for external preview bots so ordinary media does not traverse your Worker/origin.
- Can cache application-generated `compact=av4` MP4 responses at Cloudflare's edge.
- Can proxy same-origin browser media through Cloudflare while validating that redirect targets stay on trusted Instagram CDN hosts.

The Worker does **not** implement the Telegram 20 MiB decision itself. That remains application logic: Telegram-specific oversized Reels are routed to `compact=av4`; other clients can continue receiving their normal media path.

## Setup

1. Copy `wrangler.example.jsonc` to `wrangler.jsonc`.
2. Set `ORIGIN` to the HTTPS origin where your InstaFix application is reachable, for example `https://origin.your-domain.example`.
3. Add your own Cloudflare route or custom-domain configuration.
4. Deploy with Wrangler.
5. Point only the public hostname(s) you want at the Worker. Keep any special subdomains/routes required by your own deployment separate if they use different semantics.

Do not copy account IDs, zone IDs, production domains, API tokens, or other deployment-specific secrets from somebody else's setup.

## Production note

This example is intentionally generic and contains no production domain routing, diagnostics, account IDs, or deployment secrets. Use a hostname you control and check that it doesn't infringe another company's trademark. The previous public demo is unavailable.
