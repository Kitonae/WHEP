NDI → WHEP Server (Python)

Overview

- Serves a WHEP endpoint (`POST /whep`) that answers WebRTC offers with a media stream.
- Primary source is an NDI receiver (requires NewTek NDI SDK and Python bindings).
- Falls back to a synthetic test video if NDI is not available.

Quick Start

1) Install dependencies (system must have Python 3.9+):

   pip install -r requirements.txt

2) Run the server (synthetic video by default):

   python -m app --host 0.0.0.0 --port 8000 --fps 30 --width 1280 --height 720

3) Preview in the built-in web player:

   - Open http://localhost:8000/player in your browser and click Play.
   - Or use the minimal test page at http://localhost:8000/.

4) Use with any WHEP client:

   - WHEP endpoint: http://localhost:8000/whep
   - Client creates an SDP offer and POSTs it as `application/sdp`.
   - Server replies `201 Created` with SDP answer and a `Location` header for the session.

Environment Variables

- `NDI_SOURCE`: Name of the NDI source to receive (enables NDI).
- `ICE_SERVERS`: Comma-separated STUN/TURN URLs (e.g., `stun:stun.l.google.com:19302`).
- `FPS`, `VIDEO_WIDTH`, `VIDEO_HEIGHT`: Synthetic source configuration.
- `PORT`, `HOST`: Server bind address.
- `LOG_LEVEL`: Logging level (e.g., `INFO`, `DEBUG`).

NDI Integration

- A minimal ctypes-based integration is provided in `app/ndi_ctypes.py` and wired into `app/tracks/ndi_track.py`.
- Place `Processing.NDI.Lib.x64.dll` at the repository root (already present) or ensure the NDI library is available on the system path.
- Discover available sources via HTTP: `GET /ndi/sources` → `{ "sources": ["name1", ...] }`.
- Select a source by setting `NDI_SOURCE` to a substring of the NDI name (case-insensitive).
  - When selecting via API, the server stores a stable NDI URL for that source and uses it for new sessions.

Runtime control endpoints

- `GET /ndi/sources`: Lists discoverable NDI source names.
- `GET /ndi/sources/detail`: Lists discoverable sources with URLs, as `[ { name, url } ]`.
- `GET /frame`: Captures a single frame from the current source and returns a PNG image (`image/png`). Optional `?timeout=ms` (default 2000).
- `POST /ndi/select` with body `{ "source": "<substring>" }`: Sets the active NDI source used for new WHEP sessions. Validates against current discovery (best-effort) and returns `{ ok, selected, matched, available }`.
  - Response includes a `url` field when resolved, which the server uses to lock to the chosen source.
- `POST /ndi/select_url` with body `{ "url": "ndi://..." }`: Locks selection by NDI URL, bypassing name-based discovery when starting new sessions.
- `GET /health` (alias: `/healt`): Returns `{ status, sessions: { count, states }, ndi: { selected, selected_url, available } }`.

WHEP Semantics Implemented

- `POST /whep` with SDP offer in body → `201 Created` + SDP answer in body. `Location` header gives the session URL for PATCH/DELETE.
- `PATCH /whep/{id}` accepts trickle ICE payloads but currently no-ops (sufficient for most non-trickle clients).
- `DELETE /whep/{id}` closes the session.

Notes

- Audio is not wired by default. The server sends video; you can add audio by implementing an audio track and attaching it in `app/media.py`.
- This server uses aiortc’s default ICE. For public networks configure `ICE_SERVERS` with your STUN/TURN.
- NDI is only available when the process can load the NDI runtime. On Windows, the provided `Processing.NDI.Lib.x64.dll` suffices; on Linux/mac, install the appropriate NDI runtime so the shared library can be found.

Standalone Player

- A single-file player is provided at `standalone-player.html`. You can open it directly in a browser or host it on any static host (GitHub Pages, S3, nginx, etc.).
- Set the endpoint in the UI or via URL, for example:
  - `file:///.../standalone-player.html?endpoint=http://localhost:8000/whep`
  - Add ICE servers via `ice` param (comma-separated): `?ice=stun:stun.l.google.com:19302`
- When hosting over HTTPS, ensure your WHEP endpoint and ICE servers are also accessible over HTTPS and that CORS is allowed by the WHEP server.
