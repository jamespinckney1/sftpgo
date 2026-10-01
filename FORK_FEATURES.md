# WebClient enhancements (fork)

This fork adds six **additive, backward-compatible** features to the end-user
WebClient file browser. Nothing in the SFTP/FTP/WebDAV protocols, authentication,
crypto, or the virtual-filesystem sandbox is changed; the features live in the
web/HTTP layer (plus an opt-in background photo indexer) and reuse the existing
per-user permission checks. Existing endpoints keep their behavior — only new
routes and new optional query params / config keys were added.

## 1. Drag-and-drop move

Drag a file or folder row onto another folder to move it there, or onto the
breadcrumb to move it up one level. It reuses the existing
`/web/client/file-actions/move` endpoint and its task-status polling — there is
**no** new backend code. It is only enabled for users with rename permission and
is automatically disabled while viewing search results (whose items span
folders).

## 2. Recursive search (wildcard + partial)

The toolbar search box now searches the current directory **and everything
beneath it**.

- New endpoint: `GET /web/client/search?path=<start dir>&q=<query>`
- Wildcard/glob matching when `q` contains `*` or `?` (via `path.Match`),
  otherwise case-insensitive substring ("partial") matching.
- Results use the exact same JSON row shape as `/web/client/dirs`, with an extra
  `path` field naming each hit's parent folder. A "Location" column is revealed
  in the UI so results are navigable.
- The walk goes through the user's permissions per directory and is bounded by
  safety caps (5000 directories / 2000 results) so recursive listing on cloud
  backends (S3/GCS/Azure) can't run away; when a cap is hit the walk stops early
  and logs it, returning partial results.

## 3. Photo/video thumbnails + grid view

Image and (when ffmpeg is available) video files show small cached thumbnails in
the listing, and a toolbar toggle switches to a gallery/grid view. Clicking a
photo in the grid opens the existing lightbox.

- New endpoint: `GET /web/client/thumbnail?path=<file>` returns a ~200px JPEG.
- Files are always opened **through the user's connection**, so the storage
  backend and sandbox are respected. Any failure returns `404` and the UI falls
  back to a generic icon — the main listing never depends on this endpoint.
- Supported image sources: `jpg`, `jpeg`, `png`, `gif`, `webp`, `bmp`.
- Video sources (`mp4`, `mov`, `mkv`, `webm`, `avi`, `m4v`) require **ffmpeg**.
  If ffmpeg is not found, video thumbnails are disabled and videos fall back to
  a generic icon; images still work.
- Thumbnails are cached on disk keyed by user + path + mod time + size, and
  served with `ETag`/`If-None-Match` (304) support.

### ffmpeg (optional, for video thumbnails)

Install ffmpeg and make sure it is on `PATH` (or set `ffmpeg_path`). For example:

```sh
# Debian/Ubuntu
apt-get install -y ffmpeg
# macOS
brew install ffmpeg
```

### Configuration (`sftpgo.json` → `httpd.thumbnails`)

All keys are optional; thumbnails are enabled by default.

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Globally enable/disable thumbnail generation. |
| `cache_dir` | `""` | Cache directory (absolute or relative to the config dir). Empty → `<os temp dir>/sftpgo-thumbnails`. |
| `cache_max_size` | `512` | Max total cache size in MB; oldest thumbnails are evicted past this. `0` = unlimited. |
| `max_source_size` | `100` | Max source file size in MB to thumbnail; larger files fall back to an icon. |
| `ffmpeg_path` | `""` | Path to the ffmpeg binary. Empty → look it up in `PATH`. |

Every key can also be set via environment variable, e.g.
`SFTPGO_HTTPD__THUMBNAILS__ENABLED=false`.

## 4. Photo index: search by date taken (phase 1)

An opt-in background indexer catalogs the photos and videos on **local**
filesystems so the WebClient can search them by the date they were *taken*, and
show HEIC/TIFF/RAW photos, which browsers can't display. It is the groundwork for
the later phases (searching by what is pictured, and by person).

### Searching

Type photo filters in the normal search box; they can be combined with a name
(substring or `*`/`?` wildcard) and apply to the current folder and below:

| Query | Finds |
| --- | --- |
| `taken:2023` | photos taken in 2023 |
| `taken:2023-06` / `taken:2023-06-14` | a month / a day |
| `taken:2023-06..2023-08` | an inclusive range; `taken:..2010` and `taken:2020..` work too |
| `taken:any` | every indexed photo/video, newest first |
| `taken:unknown` | files with no date in their metadata or name |
| `beach taken:2021` | files named `*beach*` taken in 2021 |

Results show a **Taken** column (newest first) and work in grid view. The date is
the local time the camera recorded, so `taken:2023-06` means June 2023 wherever
the photo was taken. Where it comes from, most reliable first:

1. EXIF `DateTimeOriginal` (photos, including HEIC), Apple QuickTime
   `CreationDate` (iPhone videos), XMP, EXIF/QuickTime `CreateDate`, PNG
   `CreationTime`;
2. a date in the file name (`IMG_20230614_102233.jpg`, `PXL_…`,
   `2023-06-14 10.22.33.jpg`, `IMG-20230614-WA0001.jpg`, …);
3. otherwise the file modification time, shown greyed out with a `?`.

A small "Indexing photos…" line next to the search box shows progress while a
pass is running.

### How it works

- The index is keyed by the **real filesystem path**, not by user: a photo in a
  folder shared by several users (a virtual folder) is processed once. Every
  user's home directory and local virtual folders are indexed; cloud (S3/GCS/
  Azure/SFTP) and encrypted filesystems are skipped.
- **Access control happens at query time**: each hit is mapped back to the
  searching user's virtual path and checked with the same permission and
  file-pattern rules as a directory listing, so users only ever see their own and
  shared photos.
- Uploads, renames, moves, copies and deletes done through SFTPGo (any protocol)
  update the index immediately; renamed folders keep their data instead of being
  re-processed. A periodic rescan (default every 24h) picks up changes made
  outside SFTPGo. A scan never deletes index entries for a folder it cannot read
  (e.g. an unmounted disk).
- Each photo is decoded **once** into a ~1024px JPEG preview stored with the
  index; thumbnails and the viewer use it, and the later ML phases will too.
- Background work runs at low CPU and disk (idle I/O class) priority, one file at
  a time by default, and resumes where it stopped after a restart.
- New endpoints: `GET /web/client/photoindex/status` (progress) and
  `GET /web/client/thumbnail?size=preview&path=…` (large preview for the viewer).
  Photo filters go through the existing `/web/client/search` endpoint.

### Tools

- **exiftool** reads dates and metadata from every format (HEIC, JPEG, PNG,
  RAW, MP4/MOV). A single long-running process is reused for all files.
- **libvips** (`vipsthumbnail`) generates the previews; HEIC needs libheif with
  the libde265 plugin. RAW previews use the JPEG embedded by the camera.

Both are installed in the Docker image (build arg `INSTALL_PHOTO_TOOLS`, default
`true`). Without exiftool, dates come from file names/modification times only;
without libvips, no previews are generated. On Debian/Raspberry Pi OS:

```sh
apt-get install -y libvips-tools libheif-plugin-libde265 libimage-exiftool-perl
```

### Configuration (`sftpgo.json` → `httpd.photo_index`)

The index is **disabled by default**.

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Enable the photo index. |
| `data_dir` | `"photoindex"` | Index database and previews (absolute or relative to the config dir). Put it on an SSD. Previews take about 100–200 KB per photo (roughly 5–10 GB per 50,000 photos). |
| `preview_size` | `1024` | Longest edge, in pixels, of the generated previews. `0` disables previews. |
| `rescan_interval` | `24` | Hours between full rescans. `0` = only at startup. |
| `startup_delay` | `60` | Seconds to wait after startup before the first scan. |
| `workers` | `1` | Files processed in parallel. Keep `1` on a Raspberry Pi. |
| `pause_between_files` | `0` | Extra pause, in ms, after each file to reduce load further. |
| `max_source_size` | `200` | Max file size in MB for which a preview is generated (dates are still read). |
| `exiftool_path` | `""` | Path to exiftool. Empty → look it up in `PATH`. |
| `vips_path` | `""` | Path to `vipsthumbnail`. Empty → look it up in `PATH`. |

Every key can be set via environment variable, e.g.
`SFTPGO_HTTPD__PHOTO_INDEX__ENABLED=true`.

### Example: Raspberry Pi with photos on a HDD and the index on an SSD

```yaml
services:
  sftpgo:
    image: your-sftpgo-fork-image
    environment:
      SFTPGO_HTTPD__PHOTO_INDEX__ENABLED: "true"
      SFTPGO_HTTPD__PHOTO_INDEX__DATA_DIR: /var/lib/sftpgo/photoindex
    volumes:
      - /mnt/hdd/sftpgo-data:/srv/sftpgo/data          # photos (read by the indexer)
      - /mnt/ssd/sftpgo-photoindex:/var/lib/sftpgo/photoindex  # index + previews
      # ... your existing volumes
```

The SSD directory must be writable by the container user (uid 1000):
`sudo chown 1000:1000 /mnt/ssd/sftpgo-photoindex`.

### Measuring it on your hardware

`photoindex-bench` runs the pipeline on a sample of your files, without writing
to the index, and estimates how long the first full pass will take:

```sh
docker exec -it sftpgo sftpgo photoindex-bench --dir /srv/sftpgo/data --samples 100
```

## 5. Cleanup: largest files and duplicate photos

Two more kinds of search help free space. Like every search they apply to the
current folder and everything below it, and list only what the user can see.

### Largest files (any file type, photo index not needed)

| Query | Finds |
| --- | --- |
| `sort:size` | everything, largest first |
| `larger:500MB` / `smaller:1MB` | files of at least / at most that size (B, KB, MB, GB, TB; 1 KB = 1024 bytes) |
| `*.mov larger:1GB` | combined with a name |
| `taken:2019 larger:20MB` | combined with photo filters (photos/videos only, from the index) |

Size searches return the 2000 largest matching files and a summary line with
their count and total size.

### Duplicate photos (needs the photo index)

| Query | Finds |
| --- | --- |
| `is:duplicate` | byte-identical copies |
| `is:similar` | photos that look the same: identical copies plus resized, re-compressed or re-exported ones (a WhatsApp copy, a JPEG exported from a HEIC, …) |

Results are grouped, the groups wasting the most space first. In each group the
copy suggested to **keep** comes first: the highest resolution, then the
largest file, then the oldest, then the shortest path. **Select extra copies**
selects every other copy; review them (the grid view is handy for comparing)
and use the normal Delete action. A summary line shows how much space that
frees. Nothing is ever deleted automatically, and deletes go through the usual
permission checks.

How it works:

- **Exact duplicates**: only files that have the same size as another file
  are hashed (SHA-256), so on a real library almost nothing is read twice.
  Hashing runs in the background when the indexer is otherwise idle.
- **Similar photos**: each photo gets a 64-bit perceptual hash (dHash) computed
  from its preview, a fingerprint that survives resizing and re-compression.
  Photos whose fingerprints differ by at most 6 bits and that have the same
  aspect ratio are grouped. Featureless images (plain sky, dark frames) are
  skipped, since they would all look alike.
- **Intentional pairs are protected**: files with the same name and a different
  extension in the same folder (camera RAW+JPEG, iPhone HEIC+JPG) are never
  grouped together, and are always kept with the copy they belong to.
- Similar is not identical: bursts of nearly identical shots can be grouped,
  so review `is:similar` results before deleting.

Actions on search results (delete, rename, move/copy, download, share) now act
on the file where it actually is. Previously they assumed every result was in
the current folder.

## 6. People: face recognition

With face recognition enabled, the photo index finds the faces in every photo
and groups them by person. A **People** page in the WebClient menu lets anyone
in the family put names on the groups, and the search box finds photos by
person.

### Using it

- **People page**: named people first, then the unnamed groups ("Who is
  this?"), the largest first. Open a group to:
  - **name it**: names are shared by everybody. Giving a group the name of an
    existing person merges the two (after a confirmation);
  - **View photos**: opens the file list searching `person:#<id>`;
  - fix mistakes: select faces and use **Not this person**, or type the right
    name and **Move** them (a new name creates the person). Faces moved by hand
    are never moved again automatically;
  - **Hide** a group (strangers in the background): its future look-alikes
    keep going to it, out of the way. "Show hidden" brings it back.
- **Search**:

| Query | Finds |
| --- | --- |
| `person:rose` | photos of the people whose name contains "rose" |
| `person:"Grandma Rose"` | names with spaces |
| `person:rose person:bob` | photos showing both |
| `person:rose taken:2019..2021` | combined with any other filter |

New photos are processed automatically: a face matching a named person goes to
that person, others to the unnamed groups.

### Privacy

Names are shared, but every user only sees the people, faces, face thumbnails
and photos in the files they can access, with the same rules as a directory
listing. A person appears for a user only if at least one of their photos is
visible to that user. Face data never leaves the server: the models run
locally in the machine-learning container.

### How it works

- The faces are found by the machine-learning container of the
  [Immich](https://immich.app) project, a separate container that SFTPGo calls
  over HTTP. It runs on the CPU (arm64 images available) and downloads its
  models (about 300 MB for `buffalo_l`) from Hugging Face on first use.
- Each photo's preview is sent once, after the photo is indexed and while the
  indexer is otherwise idle, so the HDD is not read again. The face boxes and
  their 512-number descriptors are stored in the index on the SSD (about 2 KB
  per face).
- Each face joins the person whose faces it is most similar to (cosine
  similarity of the descriptors to the person's average) if the similarity
  reaches `face_match_threshold`, otherwise it starts a new unnamed group.
  Faces smaller than 36 pixels in the preview or detected with low confidence
  are ignored.
- If the container is down, faces are retried later; nothing is lost.
- Licensing: the Immich machine-learning code is AGPL-3.0 like this fork; the
  InsightFace face models (`buffalo_l`, `buffalo_s`) are licensed for
  non-commercial use only.

### Setup (Docker Compose)

Add the machine-learning container next to SFTPGo, with its model cache on the
SSD:

```yaml
  immich-ml:
    image: ghcr.io/immich-app/immich-machine-learning:release
    container_name: immich-ml
    restart: unless-stopped
    volumes:
      - /mnt/ssd/immich-ml-cache:/cache
    environment:
      - MACHINE_LEARNING_MODEL_TTL=600   # unload the models after 10 idle minutes
```

and point SFTPGo at it:

```yaml
      - SFTPGO_HTTPD__PHOTO_INDEX__ML_URL=http://immich-ml:3003
```

Check the setup with one photo before restarting SFTPGo (the first run
downloads the models):

```sh
docker exec sftpgo sftpgo photoindex-facecheck --file /srv/fileshare/some/photo.jpg
```

### Configuration (`httpd.photo_index`)

| Key | Default | Meaning |
| --- | --- | --- |
| `ml_url` | `""` | URL of the machine-learning container. Empty disables face recognition. |
| `face_model` | `"buffalo_l"` | Face model: `buffalo_l` (most accurate) or `buffalo_s` (faster, lighter). |
| `face_min_score` | `0.7` | Minimum detection confidence (0-1). |
| `face_match_threshold` | `0.5` | Minimum similarity (0-1) to add a face to a person automatically. Raise it if different people get mixed up, lower it if the same person is split into many groups. |

## License & attribution

This is an unofficial fork of [drakkan/sftpgo](https://github.com/drakkan/sftpgo),
distributed under the same **AGPL-3.0-only** license, and is not affiliated with or
endorsed by the SFTPGo project. See [NOTICE.md](NOTICE.md) for full attribution,
licensing (including the KeenThemes WebUI template terms), trademark, and
no-warranty details.
