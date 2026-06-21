# logs2cookies-bot

Telegram bot in Go. Send a `.zip` stealer log → get a merged Netscape `cookies.txt` plus a `cookies.json` back, with stats.

## Features

- accepts zip/rar archives up to 2 GB via MTProto (no Bot API 20 MB download cap), plus direct URL downloads up to 5GB
- finds cookie files across nested folders (any browser layout)
- parses Netscape format **and** JSON cookie dumps (EditThisCookie / extension exports)
- dedupes by `(domain, path, name)`
- optional domain filter via message caption: `filter:netflix.com` or just `netflix.com`
- per-job tempdir, auto-cleaned after 30 min
- concurrent jobs (one goroutine per message)

## Run

```bash
go mod tidy
export TELEGRAM_API_ID=12345678          # from https://my.telegram.org/apps
export TELEGRAM_API_HASH=your_api_hash
export TELEGRAM_BOT_TOKEN=123456:ABC...  # or BOT_TOKEN
go run .
```

## Build

```bash
go build -ldflags="-s -w" -o logs2cookies.exe
```

## Usage

1. DM the bot
2. Send a `.zip` log as a file
3. (optional) caption: `filter:steam` to keep only cookies whose domain contains `steam`
4. Bot replies with `cookies.txt` (Netscape) + `cookies.json` + stats

## Limits

- archive (telegram upload): 2 GB via MTProto
- single inner file: 50MB
- URL downloads must resolve to a real `.zip` or `.rar`; Cloudflare/browser challenge pages are rejected with a clear error

## Layout

Single file: `main.go` (~400 lines).
