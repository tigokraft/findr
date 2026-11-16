# findr

A modern, high-performance music collection manager, monitor, and automated downloader written in **Go**.

`findr` is designed as a fast, lightweight alternative to Lidarr that **ditches MusicBrainz** in favor of modern streaming catalog indexers (Deezer, Qobuz, Spotify), downloads lossless FLAC & Hi-Res audio via **Qobuz** or **Soulseek (slskd)**, verifies downloaded tracks against canonical metadata, organizes them into your library, and automatically triggers rescans on your **Navidrome** server.

---

## Why Ditch MusicBrainz?

MusicBrainz has historically caused major pain points for music collectors:
* **Convoluted Hierarchy**: Release Groups, Releases, Mediums, and Recordings result in hundreds of duplicate regional editions.
* **Strict 1 req/sec Rate Limiting**: Discography syncing is slow and prone to timeouts without a local 50GB PostgreSQL mirror.
* **Delayed / Missing Releases**: Modern digital-only releases, singles, EPs, and underground music often take weeks or months to be cataloged.
* **Fragile Matching**: Rejection of valid audio files because a physical CD release medium from 2004 didn't match modern streaming track counts.

### findr's Indexer Solution

`findr` uses a **pluggable indexer architecture**:
* **Deezer (Default, Zero-Auth)**: Instant search and discography with clean, native `record_type` breakdown (`album`, `ep`, `single`, `compilation`), ISRCs, track durations, and 1000x1000 cover art. Requires **no developer account or API keys**.
* **Qobuz Catalog**: Ideal for Qobuz downloads, providing 1:1 match guarantees, Hi-Res audio flags (24-bit/192kHz), and digital booklet metadata.
* **Spotify Web API**: Gold standard modern music catalog via standard Client Credentials.

---

## Architecture Pipeline

```
[Indexer: Deezer/Qobuz] ──> [Downloader: Qobuz/slskd]
                                    │
                                    ▼
                         [Verification Engine]
                         - Track count validation
                         - Duration tolerance (±4s)
                         - Fuzzy title similarity
                                    │ (Passed)
                                    ▼
                         [Importer & Organizer]
                         - Canonical folder/file renaming
                         - Cover art downloading (cover.jpg)
                                    │
                                    ▼
                         [Navidrome Server]
                         - Trigger /rest/startScan via Subsonic API
```

---

## Features

- **Blazing Fast Go Binary**: Standalone binary compiled with pure Go SQLite (`modernc.org/sqlite`), zero CGO requirement.
- **Zero-Config Metadata**: Start searching and indexing artists immediately via Deezer without registering API keys.
- **Qobuz Downloader**: Lossless CD quality (16-bit/44.1kHz) and Hi-Res (24-bit up to 192kHz) FLAC downloading directly from Qobuz with signed media requests.
- **Soulseek / slskd Downloader**: Soulseek P2P integration via slskd's REST API for rare releases and underground music.
- **Verification Engine**: Strict matching between downloaded files and indexer metadata (validating track counts, title similarity, and track durations) before moving into the library.
- **Library Organization**: Flexible naming templates (`{Artist}/{Album} ({Year})/{Track:02d} - {Title}.flac`) with OS-safe path sanitization.
- **Navidrome Integration**: Automatic Subsonic API notifications (`startScan`) with secure MD5 salt + token authentication.

---

## Getting Started

### 1. Docker Deployment (Recommended)

`findr` is designed to run directly in your Docker media stack alongside **Navidrome**.

```bash
# Clone the repository
git clone https://github.com/findr-app/findr.git
cd findr

# Start findr and Navidrome together
docker compose up -d
```

- **findr Web Dashboard**: `http://localhost:8686`
- **Navidrome Web Player**: `http://localhost:4533`

#### Qobuz User Token Configuration
You can pass your Qobuz `user_auth_token` directly via environment variable in `docker-compose.yml`:
```yaml
environment:
  - FINDR_QOBUZ_USER_TOKEN=your_extracted_user_auth_token_here
  - FINDR_NAVIDROME_URL=http://navidrome:4533
  - FINDR_NAVIDROME_USER=admin
  - FINDR_NAVIDROME_PASSWORD=your_password
```
*(You can obtain your token by inspecting network requests on the Qobuz web player or from your existing `qobuz-dl` config).*

---

### 2. Standalone Binary Build

Requirements: Go 1.22+

```bash
make build
cp config.example.yaml config.yaml
./bin/findr daemon
```

---

## CLI & Automation Usage

### Track an Artist and Get Everything
```bash
./bin/findr track "Justice"
```
Finds the artist, monitors them, indexes their complete discography (Albums, EPs, Singles), downloads every release, verifies each track against canonical metadata, imports into `/music`, and triggers a single Navidrome library scan when done.

### Search for an Artist
```bash
./bin/findr search "Daft Punk"
```

### Add and Monitor Artist Discography (Without Immediate Download)
```bash
./bin/findr add "Daft Punk"
```

### Download Artist Discography
```bash
./bin/findr download-artist <artist-id>
```

### List Monitored Artists & Releases
```bash
./bin/findr artists
./bin/findr albums <artist-id>
```

### Download, Verify, and Import a Single Album
```bash
./bin/findr download <album-id>
```

### Trigger Navidrome Scan
```bash
./bin/findr scan
```

---

## Web Dashboard

When running `findr daemon` (or inside Docker), navigate to **`http://localhost:8686`**:
- Search any artist in real-time via Deezer/Qobuz.
- Click **"⚡ Track & Get Everything"** to auto-download and import their complete discography.
- Click **"Trigger Navidrome Scan"** to update your library player anytime.

---

## Running Tests

```bash
make test
```

All packages include unit tests covering the verification engine, signature hashing, path generation, and SQLite repository.
