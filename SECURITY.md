# Security Policy

## Supported versions

The `main` branch receives security fixes.

## Reporting a vulnerability

Please open a private security advisory on GitHub if possible, or contact the maintainer through GitHub.

Do not include live Instagram cookies, tokens, private server IPs, or other secrets in public issues.

## Secret handling

- Keep Instagram cookies outside Git.
- Mount cookies into the auth helper as read-only files.
- Keep the auth helper bound to loopback or a private network only.
- Never log cookie values.
- Treat any token/cookie shown in terminal output, CI logs, screenshots, or issue comments as compromised and rotate it.

## Auth helper and proxy safety

The auth helper is optional and can make authenticated requests to Instagram. Use a dedicated account, low rate limits, and cooldowns. The video proxy is disabled by default and should only be enabled with an explicit preview-client allowlist, low concurrency, byte limits, and timeouts.
