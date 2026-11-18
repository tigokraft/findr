# findr

findr is a self-hosted music collection manager written in Go. You pick the artists, albums or Spotify playlists you want. findr finds them on streaming catalogs, downloads lossless audio from Qobuz or Soulseek, checks every file against the catalog's metadata, files it into your library and tells Navidrome to rescan.

It fills the same role as Lidarr but doesn't use MusicBrainz. Metadata comes from Deezer (no account needed) or Qobuz, so new digital releases, EPs and singles show up as soon as they are on streaming services.

- One static binary with a built-in web UI, and no CGO (SQLite via `modernc.org/sqlite`)
- Docker image with a ready-made Compose stack alongside Navidrome

---

## Contents

- [Features](#features)
- [How it works](#how-it-works)
- [Docker setup](#docker-setup)
- [Running without Docker](#running-without-docker)
- [Configuration](#configuration)
- [Command line](#command-line)
- [Development](#development)
- [License](#license)

---

## Features

**Finding music**
- Search artists and preview their full discography (albums, EPs, singles, compilations) before adding them.
- Monitor an artist and fetch everything, fetch only what's missing, or track a single album.
- Import a public Spotify playlist and queue the albums its tracks come from.
- Scan an existing music folder so findr knows what you already own.

**Downloading**
- **Qobuz**: FLAC from CD quality (16-bit/44.1 kHz) up to Hi-Res 24-bit/192 kHz, or MP3 320. If the chosen quality isn't available, findr steps down to the next one.
- **Soulseek** via [slskd](https://github.com/slskd/slskd): for releases Qobuz doesn't have. If a peer fails, findr tries other peers.
- Either downloader can be the main one with the other as a fallback, both for a whole album and for single missing tracks.

**Verifying and importing**
- Each download is checked before it reaches your library: track count, duration (±4 s by default) and how closely titles match.
- Files are renamed with templates such as `{Artist}/{Album} ({Year})/{Track:02d} - {Title}`, and cover art is saved next to them. findr can also write `album.nfo` files and M3U playlists.
- When an import finishes, findr triggers a Navidrome scan through the Subsonic API.

**Web UI** (port `8686`)
- Library, Search & Add, Spotify playlists, Queue & History, Manual Import, a file manager for the library and downloads folders, Settings (including a raw YAML editor), and System & Disks.
- Works on phones as well as desktop.

---

## How it works

```
 Indexer                 Downloader                 Verifier
 Deezer / Qobuz  ──►  Qobuz ⇄ slskd fallback  ──►  track count · duration · title match
                                                          │ pass
                                                          ▼
                       Navidrome  ◄── startScan ──  Importer
                                                     rename · move · cover.jpg · nfo/m3u
```

---

## Docker setup

There is no published image yet: Compose builds it from this repository. The included `docker-compose.yml` runs **findr** and **Navidrome** side by side, sharing one music folder.

### 1. Clone the repository

```bash
git clone https://github.com/tigokraft/findr.git
```

```bash
cd findr
```

### 2. Create the data folders

findr runs as a non-root user (UID/GID `1000`). If Docker creates the bind-mount folders itself on Linux, root owns them and findr can't write its database or your music. Create them first:

```bash
mkdir -p config music downloads
```

```bash
sudo chown -R 1000:1000 config music downloads
```

| Host path      | Container path | Purpose                                                        |
| -------------- | -------------- | -------------------------------------------------------------- |
| `./config`     | `/config`      | `config.yaml` and the SQLite database (`findr.db`)             |
| `./music`      | `/music`       | Your organised library (Navidrome mounts it read-only)        |
| `./downloads`  | `/downloads`   | Staging area for downloads that haven't been verified yet     |

To use an existing library, change `./music` in **both** services to its path, for example `/mnt/media/music:/music`.

### 3. Set your credentials

Put credentials in a `.env` file next to `docker-compose.yml`. Compose reads it automatically, and it is already listed in `.gitignore` and `.dockerignore`.

```dotenv
# Qobuz: a user auth token is the most reliable option
QOBUZ_USER_TOKEN=
# ...or let findr log in with your account
QOBUZ_USERNAME=
QOBUZ_PASSWORD=

# Navidrome: the admin account you create in step 5
NAVIDROME_USER=admin
NAVIDROME_PASSWORD=
```

To get a Qobuz token, sign in at [play.qobuz.com](https://play.qobuz.com), open your browser's developer tools and copy the `X-User-Auth-Token` header from any request to `qobuz.com/api.json`. If you already use `qobuz-dl`, the token in its config works too.

You can also skip the `.env` file and enter everything later on the Settings page.

### 4. Start the stack

```bash
docker compose up -d --build
```

- findr: <http://localhost:8686>
- Navidrome: <http://localhost:4533>

The container has a health check on `/healthz`. Check status and logs with:

```bash
docker compose ps
```

```bash
docker compose logs -f findr
```

### 5. Connect findr to Navidrome

1. Open Navidrome and create the admin account. Use the same username and password you put in `.env`.
2. In findr, go to **Settings → Navidrome**, turn on **Enable Navidrome**, then click **Test connection** and save. The URL is already set to `http://navidrome:4533` by the Compose file.

> Navidrome scans are **off by default**, and no environment variable turns them on. You must enable them in Settings (or set `navidrome.enabled: true` in `config/config.yaml`).

### 6. (Optional) Add Soulseek via slskd

Uncomment the `slskd` service in `docker-compose.yml` and set it to save into the shared downloads folder so findr can pick up finished files:

```yaml
  slskd:
    image: slskd/slskd:latest
    container_name: slskd
    restart: unless-stopped
    ports:
      - "5030:5030"
      - "50300:50300"
    environment:
      - SLSKD_SLSK_USERNAME=your_soulseek_username
      - SLSKD_SLSK_PASSWORD=your_soulseek_password
      - SLSKD_DOWNLOADS_DIR=/downloads
    volumes:
      - ./slskd-data:/app
      - ./downloads/slskd:/downloads
```

Create an API key in `slskd-data/slskd.yml` (under `web.authentication.api_keys`). Then add these variables to the `findr` service:

```yaml
      - FINDR_SLSKD_ENABLED=true
      - FINDR_SLSKD_URL=http://slskd:5030
      - FINDR_SLSKD_API_KEY=${SLSKD_API_KEY:-}
      - FINDR_SLSKD_DOWNLOAD_DIR=/downloads/slskd
      # Optional: make slskd the main downloader, with Qobuz as the fallback
      # - FINDR_DOWNLOADER_PREFERRED=slskd
```

`FINDR_SLSKD_DOWNLOAD_DIR` is where findr sees slskd's files. `./downloads/slskd` on the host appears as `/downloads` in slskd and as `/downloads/slskd` in findr. slskd's files must be writable by UID 1000, because findr moves them into the library after import.

### 7. Use the CLI inside the container

The container's working directory is `/config`, so CLI commands pick up the same config and database as the daemon:

```bash
docker compose exec findr findr track "Justice"
```

```bash
docker compose exec findr findr import-library
```

### Updating

```bash
git pull
```

```bash
docker compose up -d --build
```

Your config, database and library live in the mounted folders, so a rebuild keeps them.

### Environment variables

These override values from `config.yaml` **every time findr starts**. If you set one in Compose, changing the same setting in the web UI won't survive a restart, so manage each setting in one place only.

| Variable                          | Overrides                         |
| --------------------------------- | --------------------------------- |
| `FINDR_MUSIC_PATH`                | `library.music_path`              |
| `FINDR_DOWNLOAD_PATH`             | `library.download_path`           |
| `FINDR_QOBUZ_USER_TOKEN`          | Qobuz token (indexer and downloader) |
| `FINDR_QOBUZ_USERNAME`            | `downloader.qobuz.username`       |
| `FINDR_QOBUZ_PASSWORD`            | `downloader.qobuz.password`       |
| `FINDR_QOBUZ_APP_ID`              | Qobuz app ID                      |
| `FINDR_QOBUZ_APP_SECRET`          | Qobuz app secret                  |
| `FINDR_DOWNLOADER_PREFERRED`      | `downloader.preferred` (`qobuz` / `slskd`) |
| `FINDR_MAX_CONCURRENT_DOWNLOADS`  | `downloader.max_concurrent_downloads` (default 2) |
| `FINDR_SLSKD_ENABLED`             | `downloader.slskd.enabled` (`true` / `1`) |
| `FINDR_SLSKD_URL`                 | `downloader.slskd.url`            |
| `FINDR_SLSKD_API_KEY`             | `downloader.slskd.api_key`        |
| `FINDR_SLSKD_DOWNLOAD_DIR`        | `downloader.slskd.download_dir`   |
| `FINDR_NAVIDROME_URL`             | `navidrome.url`                   |
| `FINDR_NAVIDROME_USER`            | `navidrome.username`              |
| `FINDR_NAVIDROME_PASSWORD`        | `navidrome.password`              |

---

## Running without Docker

Requires Go 1.26 or newer.

```bash
make build
```

```bash
cp config.example.yaml config.yaml
```

```bash
./bin/findr daemon
```

Then open <http://localhost:8686>. Relative paths in `config.yaml` (`./music`, `./downloads`, `findr.db`) are resolved from the directory you start findr in.

---

## Configuration

Everything is in one YAML file, `config.yaml` (`/config/config.yaml` in Docker). If the file doesn't exist, findr starts with defaults and writes the file the first time you save on the Settings page. [`config.example.yaml`](config.example.yaml) explains every option. The main ones:

| Section       | What it controls                                                                 |
| ------------- | -------------------------------------------------------------------------------- |
| `library`     | Music and staging paths, folder and file naming templates                        |
| `import`      | Move vs. copy, deleting staging folders, removing empty folders                  |
| `export`      | Cover art (`cover.jpg`), `album.nfo`, M3U playlists                              |
| `indexer`     | Metadata source: `deezer` (default, no account) or `qobuz`; Spotify credentials for playlist import |
| `downloader`  | Preferred downloader, Qobuz quality (`flac_24_192`, `flac_24_96`, `flac_16_44`, `mp3_320`), slskd |
| `verifier`    | Duration tolerance, minimum confidence score, whether partial albums are allowed |
| `navidrome`   | Subsonic URL, credentials, token auth, enable/disable scans                      |

Spotify credentials are optional. Without them, playlist import reads Spotify's public embed page, which shows at most 100 tracks per playlist.

---

## Command line

```
findr <command> [arguments] [--config path]
```

| Command                    | What it does                                                             |
| -------------------------- | ------------------------------------------------------------------------ |
| `daemon`                   | Run the web UI, HTTP API and download queue                              |
| `search <query>`           | Search for an artist on the configured indexer                           |
| `add <artist>`             | Monitor an artist and index their releases without downloading           |
| `track <artist>`           | Monitor an artist and download their whole discography                   |
| `artists`                  | List monitored artists                                                   |
| `albums <artist-id>`       | List an artist's indexed releases                                        |
| `download <album-id>`      | Download, verify and import one release, then scan Navidrome             |
| `download-artist <id>`     | Download every missing release for an artist                             |
| `download-all`             | Download missing releases for every monitored artist                     |
| `import-library`           | Scan the music folder and register the artists and albums already there  |
| `scan`                     | Trigger a Navidrome library scan                                         |
| `version`                  | Print the version                                                        |

`--config` works only with commands that take no argument, e.g. `findr daemon --config /etc/findr.yaml`. Commands that take an argument (`track`, `download`, …) always read `config.yaml` from the current directory, so run them from the folder your config is in. In Docker that folder is `/config`.

---

## Development

```bash
make test
```

Code layout:

```
cmd/findr            CLI entry point
internal/indexer     Deezer and Qobuz catalog clients
internal/downloader  Qobuz and slskd downloaders, plus the fallback logic
internal/verifier    Checks downloaded files against catalog metadata
internal/importer    Renaming, tagging, cover art, nfo/m3u
internal/orchestrator  Download queue, library sync, playlist import
internal/server      HTTP API and the web UI (internal/server/web)
internal/storage     SQLite storage
```

---

## License

[MIT](LICENSE)

findr is meant for downloading music you have the right to access. You are responsible for following Qobuz's terms of service and the copyright laws where you live.
