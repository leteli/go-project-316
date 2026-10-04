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
- `--IndentJSON` (default `false`): pretty-print the JSON report with indents. Only whitespace changes; the content and key order stay the same.

## Report

The report is printed to stdout as JSON exactly as returned by the library, followed by a single trailing newline; no other text is printed before or after it. Errors are written to stderr.

```json
{
  "root_url": "https://example.com",
  "depth": 1,
  "generated_at": "2024-06-01T12:34:56Z",
  "pages": [
    {
      "url": "https://example.com",
      "depth": 0,
      "http_status": 200,
      "status": "ok",
      "error": "",
      "seo": {
        "has_title": true,
        "title": "Example title",
        "has_description": true,
        "description": "Example description",
        "has_h1": true
      },
      "broken_links": [
        {
          "url": "https://example.com/missing",
          "status_code": 404,
          "error": "Not Found"
        }
      ],
      "assets": [
        {
          "url": "https://example.com/static/logo.png",
          "type": "image",
          "status_code": 200,
          "size_bytes": 12345,
          "error": ""
        }
      ],
      "discovered_at": "2024-06-01T12:34:56Z"
    }
  ]
}
```

All keys are always present. String fields may be empty (for example, `error` is `""` when there is no error), and lists are `[]` rather than `null`. Timestamps are in ISO 8601 format.

### Top level

- `root_url`: the start URL as passed to the crawler.
- `depth`: the maximum crawl depth (`--depth`).
- `generated_at`: when the report was generated.
- `pages`: crawled pages of the site, sorted by depth and then by URL.

### Page

- `url`: page URL.
- `depth`: how many links away from the start page the page is; the start page has depth `0`.
- `http_status`: HTTP status code of the page response; `0` if no response was received.
- `status`: `ok` if the page was loaded and parsed, otherwise `error`.
- `error`: why the page failed (HTTP status, network error, not an HTML page); empty on success.
- `seo`: basic SEO data of the page:
  - `has_title`, `title`: whether the page has a `<title>` and its text;
  - `has_description`, `description`: whether the page has `<meta name="description">` and its content;
  - `has_h1`: whether the page has an `<h1>`.
- `broken_links`: links from the page that failed (network error or status outside `2xx`), sorted by URL:
  - `url`: link URL;
  - `status_code`: HTTP status code; `0` for network errors;
  - `error`: error description.
- `assets`: images, scripts and stylesheets used by the page, sorted by URL. Each asset is requested once; pages that share an asset get the same data:
  - `url`: asset URL;
  - `type`: `image` (`<img>`), `script` (`<script>`) or `style` (`<link rel="stylesheet">`);
  - `status_code`: HTTP status code; `0` for network errors;
  - `size_bytes`: size from the `Content-Length` header, or the actual body size if the header is missing; `0` if the size cannot be determined;
  - `error`: error description (status `>= 400`, network error, failed body read); empty on success.
- `discovered_at`: when the page was crawled.

## Retries

A request is retried, up to `--retries` extra times, if it:

- times out;
- gets one of these HTTP statuses: `408`, `425`, `429`, `500`, `502`, `503`, `504`.

These are not retried: other network errors (connection refused or reset, DNS failures, blocked addresses),
any other HTTP status, and a canceled or expired crawl.

Retries use exponential backoff with jitter: each wait is a random time between 0 and a cap that starts at 200ms, doubles with every retry and never exceeds 5s. Retries still respect `--rps` and `--delay`.
If all attempts fail, the last response or error is reported.