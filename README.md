# komiku-cloud

<p align="center">
  <img src=".github/readme-images/logo.png" width="180" alt="komiku-cloud logo">
</p>

Self-hosted sync server for [Komikku](https://github.com/komikku-app/komikku), implementing its
built-in `SyncYomi` protocol so your reading progress, library, history, and settings sync
between your own devices — no Google account, no third party.

The server is intentionally dumb: it stores one blob per API key with ETag-based optimistic
locking. All merging of library/history/settings is done by the Komikku app itself; this just
makes sure both phones are talking to the same, consistent blob.

## Deploy to a Ubuntu VPS (Docker)

1. **Point DNS** at your server: create an A record, e.g. `sync.yourdomain.com` -> `<VPS IP>`.
2. **Install Docker** on the VPS if not already there:
   ```
   curl -fsSL https://get.docker.com | sh
   ```
3. **Get the code onto the VPS** (clone this repo, or `scp` the directory over).
4. **Configure the domain**:
   ```
   cp .env.example .env
   # edit .env, set DOMAIN=sync.yourdomain.com
   ```
5. **Start it**:
   ```
   docker compose up -d --build
   ```
   Caddy will automatically obtain a Let's Encrypt certificate for `DOMAIN` on first request —
   make sure ports 80 and 443 are open in your firewall/security group.
6. **Create an API key** (one key = one sync group; use the same key on every device you want
   synced together):
   ```
   docker compose exec app /app/komiku-cloud keygen "my-phones"
   ```
   This prints a long hex key — copy it somewhere safe (a password manager). There's no way to
   retrieve it again later, only issue a new one.

## Configure Komikku (on each phone)

Settings -> Data and storage -> Sync -> Sync Service -> **SyncYomi**:
- Host: `https://sync.yourdomain.com`
- API Key: the key from `keygen`
- Pick what to sync (library, chapters, history, tracking, settings)
- Enable periodic/automatic sync if you want it to happen in the background

Use the **same API key** on both phones — that's what makes them sync with each other.

## Operational notes

- Data lives in the `app-data` Docker volume (SQLite). Back it up if you care about not having
  to re-seed from a phone's local backup.
- `docker compose logs -f app` / `docker compose logs -f caddy` for troubleshooting.
- To issue more keys later (e.g. a third device, or revoke-by-replace), run `keygen` again and
  reconfigure that device; there's currently no revoke command — delete the row from
  `api_keys` in the SQLite DB if you need to invalidate one.
- Rate limiting is a simple 10 req/s per source IP, enforced before auth.

## Local development

```
docker compose up --build
```

Caddy needs a real resolvable `DOMAIN` to issue a certificate, so for local testing it's easier
to run just the app container directly and hit it over plain HTTP:

```
docker build -t komiku-cloud-app .
docker run -p 8080:8080 komiku-cloud-app
docker exec <container> /app/komiku-cloud keygen test
```
