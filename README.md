# trawl

A small self-hosted downloader for **HLS streams (`.m3u8`)** and **plain files**, built to fill a fast line.
It runs as one Go binary plus ffmpeg, with a single-page web UI, and needs no database.

- **Parallel downloads.** HLS segments are fetched by up to 16 workers per job. Plain files are split into byte ranges fetched in parallel, and an interrupted range resumes where it stopped. A global cap limits connections across all jobs.
- **HLS support.**
  - From a master playlist, trawl picks the best variant and adds its separate audio track.
  - Handles AES-128 encryption, fMP4 (`#EXT-X-MAP`) and byte-range playlists.
  - Given a quality-named link like `360p.m3u8`, it looks for a better sibling (`master.m3u8`, `1080p.m3u8`, …).
- **Finds the video on a page.** Give only a page URL, or paste a page link as the URL, and trawl looks for `.m3u8` playlists and video files in it: `og:video`, `<video>`/`<source>`, JSON-LD and URLs inside scripts, then up to three embedded-player iframes. It picks a playlist over a file, prefers the highest resolution in the URL, and never picks previews, trailers or ads. The other candidates are listed in the job notes.
- **Per-job headers.** A page URL becomes the `Referer` and `Origin` headers. You can add any other headers (Cookie, User-Agent, …).
- **File names.**
  - The name comes from, in order: the name you typed, the page title (`og:title` / `<title>`), `Content-Disposition`, or the URL. Meaningless names like `360p`, `index` or random tokens are skipped.
  - Names are cleaned: illegal characters and spaces are replaced, accents folded, length capped.
  - Extension rules like `.vid=.mp4` are supported, and the container is converted when needed.
- **No overwrites.** If `name.mp4` exists, the file becomes `name_1.mp4`. The final move is atomic.
- **Checks after download.**
  - Segment counts and byte counts must match. An HTML error page served instead of video is caught.
  - ffprobe checks the streams, and the duration must match the playlist.
  - An optional full decode checks every frame.
  - Files that fail are kept in `DATA/failed/` for inspection.
- **Duplicate detection.** A sha256 is computed for every file. A file identical to one already in the folder is held aside instead of saved. **Keep as copy** saves it under a new name instantly, with no re-download, and **Remove** deletes it.
- **Optional.** Notifications to Discord or ntfy, and a basic-auth password.

## Run

```sh
podman run -d --name trawl -p 8080:8080 \
  -v ./data:/data:Z \
  -v /mnt/media/videos:/downloads:Z \
  ghcr.io/503stack/trawl:latest
```

The container runs as uid/gid 1000, so the mounted folders must be writable by that user. To run as someone else, use `--user`.

Open http://localhost:8080. Paste one or more URLs, optionally with the page they came from, and press Download.

Use the **bookmarklet** link in the UI on a video page, preferably once the video is playing. It opens trawl in a new tab with the page URL and title filled in, together with the source of the playing `<video>` (any extension, e.g. `.vid`) and the playlists and video files the browser has loaded. Press Download with the URL field empty and trawl picks the best one. This also finds streams that the player builds in JavaScript, which a scan of the HTML would miss.

### API

```sh
curl -X POST localhost:8080/api/jobs -H 'Content-Type: application/json' -d '{
  "url": "https://cdn.example/stream/720p.m3u8",
  "page_url": "https://example.com/watch/123",
  "name": "optional name",
  "folder": "downloads",
  "headers": "Cookie: a=b\nUser-Agent: ...",
  "verify": "probe",
  "upgrade": true,
  "dedupe": "skip"
}'
```

| Endpoint | |
|---|---|
| `GET /api/jobs` | all jobs, newest first, with live progress |
| `POST /api/jobs/{id}/cancel` | cancel a queued or running job |
| `POST /api/jobs/{id}/keep` | body `{"name": "..."}` (optional): save a held duplicate anyway |
| `POST /api/jobs/{id}/retry` | body `{"url": "..."}` (optional) to retry with a fresh link |
| `DELETE /api/jobs/{id}` | remove from the list (the file stays) |
| `POST /api/clear` | remove finished jobs |
| `GET /healthz` | health check |

`url` may be left out when `page_url` is set. trawl then picks the video from `candidates` (URLs seen in the browser) and the page itself, and records the choice in `found_url`. A plain retry searches the page again, which helps with expiring links.

Mutating requests must send `Content-Type: application/json`.

## Configuration

All settings are environment variables.

| Variable | Default | |
|---|---|---|
| `TRAWL_LISTEN` | `:8080` | |
| `TRAWL_DATA` | `/data` | `jobs.json`, `tmp/`, `failed/`, `duplicates/` |
| `TRAWL_TEMP` | `$TRAWL_DATA/tmp` | Work area. Put it on the same filesystem as the folders to make the final move a hard link instead of a copy. |
| `TRAWL_FOLDERS` | `downloads:/downloads` | `name:/path;name2:/path2`. The first one is the default. |
| `TRAWL_PASSWORD` | | Enables HTTP basic auth (any user name). |
| `TRAWL_NOTIFY_URL` | | Discord webhook URL, or any URL that accepts a text POST (ntfy). |
| `TRAWL_MAX_JOBS` | `2` | Jobs running at the same time. |
| `TRAWL_MAX_CONNECTIONS` | `32` | Total HTTP connections across all jobs. |
| `TRAWL_JOB_CONNECTIONS` | `16` | Parallel segments or ranges per job. |
| `TRAWL_CHUNK_SIZE` | `8MiB` | Byte-range size for plain files. |
| `TRAWL_RETRIES` | `5` | Per segment or range. A 4xx is retried only once. |
| `TRAWL_USER_AGENT` | recent Chrome | |
| `TRAWL_HTTP2` | `false` | Many HTTP/1.1 connections usually beat one multiplexed HTTP/2 connection. |
| `TRAWL_IP` | `auto` | `6` tries IPv6 first, then IPv4; `4` uses IPv4 only. Use `6` when a site binds links to the browser's IPv6 address and trawl would otherwise go out over IPv4, e.g. in a container whose only IPv6 address is a ULA behind NAT66. |
| `TRAWL_NAME_SEPARATOR` | `_` | `_`, `-` or `.` |
| `TRAWL_NAME_ASCII` | `true` | Fold accents (`é→e`, `ø→o`, `ß→ss`) and drop other non-ASCII characters. |
| `TRAWL_NAME_MAX_BYTES` | `200` | |
| `TRAWL_NAME_STRIP` | | Regex removed from titles, e.g. `\s*[-\|]\s*SiteName$` |
| `TRAWL_EXT_RULES` | | `.vid=.mp4,.ts=.mp4`. If the target is a different container, the file is remuxed with `ffmpeg -c copy`. |
| `TRAWL_VERIFY` | `probe` | `transfer`, `probe` or `decode` (slow). Can be overridden per job. |
| `TRAWL_VERIFY_DURATION_TOLERANCE` | `1%` | Allowed difference from the playlist duration (at least 2 s). |
| `TRAWL_VERIFY_MIN_SIZE` | `1MiB` | Smaller media files fail. |
| `TRAWL_VERIFY_REQUIRE_AUDIO` | `true` | Video without audio fails. |
| `TRAWL_DEDUPE` | `skip` | `skip`: hold identical files in `DATA/duplicates/` until you keep or remove them. `keep`: save them as `name_1` with a note. `off`: don't check. Can be overridden per job. Held files use disk space until the job is removed or cleared. |

## How a job runs

```
download → transfer checks → remux (ffmpeg -c copy, +faststart) → ffprobe checks → sha256
        → name + extension rules → duplicate check → atomic move (name, name_1, ...)
```

HLS segments are downloaded in parallel but written in order through a bounded reorder window. Memory stays small and the data is written to disk only once.

Jobs survive restarts. A job that was running when trawl stopped starts again from the beginning. Links with expiring tokens may then fail; use **Retry with new URL**.

## Not supported (yet)

- DRM (`SAMPLE-AES`, Widevine, FairPlay)
- DASH (`.mpd`)
- Site-specific extraction: the page search is generic. URLs fetched from an API after load, `blob:` sources and pages behind a login (unless your Cookie header covers them) are not found from the server; the bookmarklet catches most of these. A yt-dlp fallback may come later.
- Live streams: trawl downloads the segments listed at the moment it reads the playlist.

## Development

```sh
go test ./...   # the end-to-end tests need ffmpeg and skip without it
go run .        # set TRAWL_DATA and TRAWL_FOLDERS to local paths
```
