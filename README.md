# Парсер сайтов (Go)

[![hexlet-check](https://github.com/leteli/go-project-316/actions/workflows/hexlet-check.yml/badge.svg)](https://github.com/leteli/go-project-316/actions)

## Usage

```
hexlet-go-crawler [flags] <url>
```

The JSON report is written to stdout.

## Flags

- `--depth` (default `10`): maximum crawl depth; `0` means the start page only.
- `--delay` (default `0s`): minimum delay between requests, e.g. `200ms`, `1s`.
- `--rps` (default `0`): maximum requests per second; overrides `--delay` when set.
- `--timeout` (default `15s`): timeout for a single request, e.g. `200ms`, `1s`.
- `--workers` (default `4`): number of concurrent workers.
- `--retries` (default `1`): number of extra attempts for a failed request; `0` disables retries.

## Retries

A request is retried, up to `--retries` extra times, if it:

- times out;
- gets one of these HTTP statuses: `408`, `425`, `429`, `500`, `502`, `503`, `504`.

These are not retried: other network errors (connection refused or reset, DNS failures, blocked addresses),
any other HTTP status, and a canceled or expired crawl.

Retries use exponential backoff with jitter: each wait is a random time between 0 and a cap that starts at 200ms, doubles with every retry and never exceeds 5s. Retries still respect `--rps` and `--delay`.
If all attempts fail, the last response or error is reported.