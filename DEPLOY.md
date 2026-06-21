# Deploy on GitHub Actions (manual, up to 6h)

## One-time setup

1. Create a **private** GitHub repo and push this folder.
2. Go to **Settings → Secrets and variables → Actions → New repository secret**
   - `BOT_TOKEN` — bot token from [@BotFather](https://t.me/BotFather)
   - `TELEGRAM_API_ID` — numeric app id from [my.telegram.org/apps](https://my.telegram.org/apps)
   - `TELEGRAM_API_HASH` — api hash from the same page

## Start the bot

1. Open the repo on GitHub → **Actions**
2. Click **Run logs2cookies bot** (left sidebar)
3. **Run workflow** → pick duration (1 / 2 / 4 / 6 hours) → **Run workflow**

The job builds from source, runs tests, then starts the bot. It stops automatically when the timer ends.

Downloads and extraction temp files go to **`D:\botdata\work`** on the Windows runner (the large temp disk — not the small `C:` OS volume).

## Notes

- Workflow uses **`windows-latest`** so archives/spool land on **`D:`** (~tens of GB free vs tight `C:`).
- GitHub-hosted runners max out at **6 hours** per job.
- Only **one** bot run at a time (new run cancels the previous).
- Runner disk is ephemeral — `work/` temp files are discarded when the job ends.
- For 24/7 hosting use a VPS instead (Railway, Hetzner, etc.).

## Push from your machine

```bash
cd logs2cookies
git init
git add .
git commit -m "logs2cookies bot with GitHub Actions runner"
git branch -M main
git remote add origin https://github.com/YOUR_USER/YOUR_REPO.git
git push -u origin main
```

Or with a token:

```bash
git remote add origin https://YOUR_TOKEN@github.com/YOUR_USER/YOUR_REPO.git
git push -u origin main
```

**Never commit tokens or API credentials into the repo** — use GitHub Secrets for `BOT_TOKEN`, `TELEGRAM_API_ID`, and `TELEGRAM_API_HASH`.
