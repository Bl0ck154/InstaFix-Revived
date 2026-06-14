# Agent workflow for InstaFix Revived

This repository is the clean public open-source base. It intentionally has no old private Git history and must not contain secrets, production-only SEO, private server paths, private domains, Telegram alerting, or production deployment traces.

## Repositories

- Public clean repo: `Bl0ck154/InstaFix-Revived`
- Private production overlay: `Bl0ck154/InstaFix-Bl0ck`

General fixes and features should be developed in the public repo first whenever possible. The private production repo may cherry-pick or merge public commits and then add private-only deployment changes.

## Public/private rules

Allowed in public:

- Generic app code.
- Generic auth-helper code with safe defaults.
- Generic Docker/Caddy examples.
- Documentation using placeholder paths and placeholder cookie values.
- Attribution to the original `Wikidepia/InstaFix` project.

Not allowed in public:

- Real cookies, tokens, `.env` files, logs, or secrets.
- Private server IPs, privileged production paths, hostnames, or rollback scripts.
- Telegram alert/report integrations from the private production overlay.
- Hardcoded production SEO routes/content for a specific live domain.
- Private analytics, private operations scripts, or production-only traffic tactics.

## Before pushing public changes

Run available tests/builds:

```sh
go test ./...
go build ./...
python -m py_compile auth-helper/app.py auth-helper/test_app.py
python -m unittest discover -s auth-helper -p 'test_*.py'
```

Run safety greps:

```sh
rg -n "TELEGRAM_|instagram7\.com|212\.227|sessionid=|csrftoken=|ds_user_id=|paypal|sponsors" .
```

Cookie field names without values are acceptable in docs. Real values are not.

## Working with the private overlay

- Public-safe changes: commit to `InstaFix-Revived`, then bring them into the private repo.
- Production-only changes: commit only to the private repo.
- If a private production change becomes generally useful, reimplement it cleanly in public without private details.

Never push private-only files from `InstaFix-Bl0ck` into `InstaFix-Revived`.
