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

### 1. Build from Source

Requirements: Go 1.22+

```bash
git clone https://github.com/findr-app/findr.git
cd findr
make build
```

### 2. Configuration

Copy the example configuration:
```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` to specify your music directory, Qobuz account credentials, and Navidrome server URL.

---

## CLI Usage

### Search for an Artist
```bash
./bin/findr search "Daft Punk"
```

### Add and Monitor Artist Discography
```bash
./bin/findr add "Daft Punk"
```
Finds the artist, adds them to your local SQLite database, and indexes all studio albums, EPs, and singles.

### List Artist Releases
```bash
./bin/findr albums <artist-id>
```

### List All Monitored Artists
```bash
./bin/findr artists
```

### Download, Verify, and Import an Album
```bash
./bin/findr download <album-id>
```
Executes the automated pipeline:
1. Fetches full tracklist and durations from indexer
2. Downloads files via Qobuz or slskd into a temporary staging folder
3. Scans and verifies audio files (scoring title, track number, and duration within tolerance)
4. Imports verified files into organized library folders and saves cover art
5. Triggers a background scan on your Navidrome server

### Trigger Navidrome Scan
```bash
./bin/findr scan
```

---

## Running Tests

```bash
make test
```

All packages include unit tests covering the verification engine, signature hashing, path generation, and SQLite repository.
