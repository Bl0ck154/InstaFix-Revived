# Contributing

Thank you for helping maintain InstaFix Revived.

## Development basics

1. Keep changes small and focused.
2. Prefer public, unauthenticated scraping behavior first.
3. Keep authenticated fallback optional, local-only, and conservative.
4. Do not add browser automation/headless scraping unless there is a clear, reviewed reason.
5. Do not commit secrets, cookies, production `.env` files, logs, or private deployment details.

## Validation

Run what is available in your environment:

```sh
go test ./...
go build ./...
python -m py_compile auth-helper/app.py auth-helper/test_app.py
python -m unittest discover -s auth-helper -p 'test_*.py'
```

Before submitting public changes, search for accidental private data:

```sh
rg -n "sessionid|csrftoken|ds_user_id|TELEGRAM_|212\.227|instagram7\.com" .
```

Cookie field names in documentation are fine; real values are never fine.

## Pull requests

- Explain the user-visible behavior change.
- Note any new environment variables.
- Include test results or explain why a test could not be run.
- Keep production-only SEO, alerting, private domains, and private server paths out of this repository.
