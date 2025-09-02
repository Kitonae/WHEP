import asyncio
import json
import logging
import os
import uuid
from typing import Dict

from aiohttp import web
from pathlib import Path
from aiortc import RTCPeerConnection, RTCSessionDescription, RTCRtpSender
from aiortc.rtcconfiguration import RTCConfiguration, RTCIceServer

from .media import build_tracks
from .ndi_ctypes import list_source_names, list_source_details, NDIError, NDI
from .ndi_cache import NDINameCache

logger = logging.getLogger(__name__)


class WhepServer:
    def __init__(self):
        self.sessions: Dict[str, RTCPeerConnection] = {}
        self.ndi_source = os.getenv("NDI_SOURCE")
        self.ndi_source_url = os.getenv("NDI_SOURCE_URL")
        self.ndi_exact_name = None
        # Shared NDI discovery cache (best-effort; falls back if unavailable)
        self.ndi_cache = NDINameCache(refresh_ms=1000)
        self._stat_tasks: Dict[str, asyncio.Task] = {}
        self._cpu_task: asyncio.Task | None = None

    async def on_startup(self, app: web.Application):
        # Start background discovery cache
        try:
            self.ndi_cache.start()
            logger.info("NDI cache started")
        except Exception as e:
            logger.warning("NDI cache failed to start: %s", e)
        # Optional CPU usage logger
        if (os.getenv("STATS_LOG") or "").lower() in ("1", "true", "yes"):
            self._cpu_task = asyncio.create_task(self._log_process_cpu())

    def _rtc_config(self) -> RTCConfiguration:
        ice_servers_env = os.getenv("ICE_SERVERS", "")
        servers = []
        for url in [u.strip() for u in ice_servers_env.split(",") if u.strip()]:
            servers.append(RTCIceServer(urls=url))
        return RTCConfiguration(iceServers=servers)

    async def create_app(self) -> web.Application:
        app = web.Application(middlewares=[cors_middleware])
        app.add_routes(
            [
                web.get("/", self.handle_index),
                web.get("/ndi/sources", self.handle_ndi_sources),
                web.get("/ndi/sources/detail", self.handle_ndi_sources_detail),
                web.get("/frame", self.handle_frame_png),
                web.post("/whep", self.handle_whep_post),
                web.options("/whep", handle_options),
                web.patch("/whep/{session_id}", self.handle_whep_patch),
                web.options("/whep/{session_id}", handle_options),
                web.delete("/whep/{session_id}", self.handle_whep_delete),
            ]
        )
        app.on_shutdown.append(self.on_shutdown)
        return app

    async def handle_whep_post(self, request: web.Request) -> web.Response:
        offer_sdp = await request.text()
        if not offer_sdp.strip():
            return web.Response(status=400, text="Empty SDP offer")

        pc = RTCPeerConnection(self._rtc_config())
        session_id = uuid.uuid4().hex
        self.sessions[session_id] = pc
        logger.info("WHEP session %s: created", session_id)

        @pc.on("connectionstatechange")
        async def on_state_change():
            logger.info("Session %s state: %s", session_id, pc.connectionState)
            if pc.connectionState in ("failed", "closed", "disconnected"):
                await self._cleanup(session_id)

        # Add media track (NDI or synthetic) with codec preferences and encoding params
        bundle = build_tracks(ndi_source=self.ndi_source, ndi_url=self.ndi_source_url, ndi_exact=self.ndi_exact_name)
        # Create a transceiver so we can set codec preferences (e.g., prefer H264 over VP8)
        transceiver = pc.addTransceiver("video", direction="sendonly")
        try:
            preferred = os.getenv("VIDEO_PREFERRED_CODEC", "H264").strip().lower()
            caps = RTCRtpSender.getCapabilities("video").codecs
            # Build a preference list: preferred codec first, then others
            def pick(kind):
                k = kind.lower()
                sel = []
                if k == "h264":
                    # Prefer packetization-mode=1 profiles
                    sel += [c for c in caps if c.mimeType.lower()=="video/h264" and "packetization-mode=1" in (c.parameters or "")]
                    sel += [c for c in caps if c.mimeType.lower()=="video/h264" and "packetization-mode=1" not in (c.parameters or "")]
                else:
                    sel += [c for c in caps if c.mimeType.lower()==f"video/{k}"]
                return sel
            ordered = []
            ordered += pick(preferred)
            # Fallbacks
            for alt in ("vp8", "h264", "vp9"):
                if alt != preferred:
                    ordered += [c for c in caps if c not in ordered and c.mimeType.lower()==f"video/{alt}"]
            # Anything else
            ordered += [c for c in caps if c not in ordered]
            if ordered:
                transceiver.setCodecPreferences(ordered)
        except Exception:
            pass
        transceiver.sender.replaceTrack(bundle.video)
        sender = transceiver.sender
        # Optional encoder parameters to help performance
        try:
            params = sender.getParameters()
            encs = params.encodings or [{}]
            max_bitrate = os.getenv("VIDEO_MAX_BITRATE")
            max_fps = os.getenv("VIDEO_MAX_FPS")
            scale_down = os.getenv("VIDEO_SCALE_DOWN_BY")
            for enc in encs:
                if max_bitrate and max_bitrate.isdigit():
                    enc["maxBitrate"] = int(max_bitrate)
                if max_fps and max_fps.isdigit():
                    enc["maxFramerate"] = float(max_fps)
                if scale_down:
                    try:
                        enc["scaleResolutionDownBy"] = float(scale_down)
                    except ValueError:
                        pass
            params.encodings = encs
            sender.setParameters(params)
        except Exception:
            pass
        # audio is optional; not enabled by default
        # if bundle.audio:
        #     pc.addTrack(bundle.audio)

        await pc.setRemoteDescription(RTCSessionDescription(sdp=offer_sdp, type="offer"))
        answer = await pc.createAnswer()
        await pc.setLocalDescription(answer)

        # Wait for ICE gathering to complete so the SDP answer contains candidates.
        # This avoids requiring client PATCH trickle for many WHEP clients.
        if pc.iceGatheringState != "complete":
            loop = asyncio.get_running_loop()
            done = loop.create_future()

            @pc.on("icegatheringstatechange")
            async def _on_gath():
                if pc.iceGatheringState == "complete" and not done.done():
                    done.set_result(True)

            try:
                await asyncio.wait_for(done, timeout=3.0)
            except asyncio.TimeoutError:
                # Proceed anyway; some candidates are present and more might trickle.
                pass

        location = str(request.url.join(request.app.router["whep_patch"].url_for(session_id=session_id)))
        headers = {"Location": location, "Content-Type": "application/sdp"}
        # Start stats logger for this session if profiling is enabled
        if (os.getenv("STATS_LOG") or "").lower() in ("1", "true", "yes"):
            self._stat_tasks[session_id] = asyncio.create_task(self._log_sender_stats(session_id, sender))
        return web.Response(status=201, headers=headers, text=pc.localDescription.sdp)

    async def handle_whep_patch(self, request: web.Request) -> web.Response:
        session_id = request.match_info["session_id"]
        pc = self.sessions.get(session_id)
        if not pc:
            return web.Response(status=404, text="Unknown session")

        # Minimal no-op for trickle ICE; clients that rely on PATCH can send
        # application/trickle-ice-sdpfrag or JSON. We accept and ignore.
        _ = await request.text()
        return web.Response(status=204)

    async def handle_whep_delete(self, request: web.Request) -> web.Response:
        session_id = request.match_info["session_id"]
        await self._cleanup(session_id)
        return web.Response(status=204)

    async def handle_index(self, request: web.Request) -> web.Response:
        return web.Response(text=_INDEX_HTML, content_type="text/html")

    async def handle_ndi_sources(self, request: web.Request) -> web.Response:
        # Serve from cache if available; otherwise fall back to on-demand discovery
        t = request.rel_url.query.get("timeout")
        timeout_ms = int(t) if t is not None else 3000
        timeout_ms = max(0, min(timeout_ms, 15000))
        try:
            cached = self.ndi_cache.get_names()
            if cached:
                return web.json_response({"sources": cached, "cached": True})
        except Exception:
            pass
        # Fallback: do a one-shot discovery
        try:
            names = list_source_names(timeout_ms=timeout_ms)
            return web.json_response({"sources": names, "cached": False})
        except Exception as e:
            logger.exception("NDI discovery failed: %s", e)
            return web.json_response({"error": str(e)}, status=500)

    async def handle_ndi_sources_detail(self, request: web.Request) -> web.Response:
        # Serve from cache when possible for low-latency responses.
        t = request.rel_url.query.get("timeout")
        timeout_ms = int(t) if t is not None else 1000
        timeout_ms = max(0, min(timeout_ms, 10000))
        try:
            details = self.ndi_cache.get_details()
            if details:
                return web.json_response({"sources": details, "cached": True})
        except Exception:
            pass
        try:
            details = list_source_details(timeout_ms=timeout_ms)
            return web.json_response({"sources": details, "cached": False})
        except Exception as e:
            logger.exception("NDI discovery (detail) failed: %s", e)
            return web.json_response({"error": str(e)}, status=500)

    async def handle_ndi_select(self, request: web.Request) -> web.Response:
        try:
            payload = await request.json()
        except Exception:
            return web.json_response({"error": "Invalid JSON"}, status=400)

        target = payload.get("source") if isinstance(payload, dict) else None
        if not target or not isinstance(target, str):
            return web.json_response({"error": "'source' must be a string"}, status=400)

        # Validate against discovered sources (best-effort)
        try:
            available = self.ndi_cache.get_names() or list_source_names(timeout_ms=1000)
        except Exception:
            available = []
        match = None
        low = target.lower()
        for name in available:
            if low in name.lower():
                match = name
                break

        if available and match is None:
            return web.json_response({"error": "No matching NDI source found", "available": available}, status=404)

        # Try to resolve a stable URL for the selection (best-effort)
        matched_url = None
        try:
            # Prefer cache detail to resolve URL quickly
            for item in (self.ndi_cache.get_details() or []):
                name = item.get("name") or ""
                url = item.get("url")
                if low in name.lower():
                    matched_url = url
                    break
            if matched_url is None:
                # Fallback one-shot resolution
                ndi = NDI()
                srcs = ndi.find_sources(timeout_ms=1000)
                for s in srcs:
                    name = (s.p_ndi_name or b"").decode("utf-8", errors="ignore")
                    if low in name.lower():
                        matched_url = (s.p_url_address or b"").decode("utf-8", errors="ignore")
                        break
        except Exception:
            matched_url = None

        self.ndi_source = target
        self.ndi_exact_name = match
        if matched_url:
            self.ndi_source_url = matched_url
        logger.info("NDI source selected: %s (match=%s)", target, match)
        return web.json_response({"ok": True, "selected": target, "matched": match, "url": self.ndi_source_url, "available": available})

    async def handle_ndi_select_url(self, request: web.Request) -> web.Response:
        try:
            payload = await request.json()
        except Exception:
            return web.json_response({"error": "Invalid JSON"}, status=400)
        url = payload.get("url") if isinstance(payload, dict) else None
        if not url or not isinstance(url, str):
            return web.json_response({"error": "'url' must be a string"}, status=400)
        self.ndi_source_url = url
        # Keep last selected name for reference only
        logger.info("NDI URL selected: %s", url)
        return web.json_response({"ok": True, "url": url})

    async def handle_frame_png(self, request: web.Request) -> web.Response:
        """Capture a single frame from the current source and return it as PNG.
        This creates a transient track (NDI or synthetic), pulls one frame,
        encodes it via PyAV to PNG, and returns bytes with Content-Type image/png.
        """
        import av
        import asyncio
        import numpy as np
        import zlib, struct

        # Allow optional timeout override via query (?timeout=ms)
        try:
            t = int(request.rel_url.query.get("timeout", "2000"))
        except ValueError:
            t = 2000
        timeout_s = max(0.1, min(10.0, t / 1000.0))

        # Build a fresh track using current selection
        bundle = build_tracks(ndi_source=self.ndi_source, ndi_url=self.ndi_source_url, ndi_exact=self.ndi_exact_name)
        video = bundle.video
        try:
            # Receive a single frame with timeout
            frame = await asyncio.wait_for(video.recv(), timeout=timeout_s)
        except asyncio.TimeoutError:
            return web.json_response({"error": "Timed out waiting for frame"}, status=504)
        except Exception as e:
            logger.exception("/frame: failed to receive frame: %s", e)
            return web.json_response({"error": str(e)}, status=500)
        finally:
            # Ensure background tasks/receivers are cleaned up for NDI tracks
            try:
                stop = getattr(video, "stop", None)
                if callable(stop):
                    stop()
            except Exception:
                pass

        try:
            # Encode PNG without relying on container/codec availability.
            # Convert to RGB24 ndarray (H, W, 3)
            arr = frame.to_ndarray(format="rgb24")
            h, w, _ = arr.shape

            def _png_chunk(tt: bytes, dd: bytes) -> bytes:
                return struct.pack(">I", len(dd)) + tt + dd + struct.pack(">I", zlib.crc32(tt + dd) & 0xFFFFFFFF)

            # Build raw scanlines with filter type 0 for each row
            raw = bytearray()
            # Ensure contiguous C-order RGB bytes
            if not arr.flags.c_contiguous:
                arr = np.ascontiguousarray(arr)
            rgb = arr.reshape(h, w * 3)
            for y in range(h):
                raw.append(0)  # filter type 0
                raw.extend(rgb[y].tobytes())
            comp = zlib.compress(bytes(raw), level=6)

            sig = b"\x89PNG\r\n\x1a\n"
            ihdr = struct.pack(
                ">IIBBBBB",
                w,
                h,
                8,   # bit depth
                2,   # color type: truecolor RGB
                0,   # compression method
                0,   # filter method
                0,   # interlace method
            )
            data = sig + _png_chunk(b"IHDR", ihdr) + _png_chunk(b"IDAT", comp) + _png_chunk(b"IEND", b"")
            headers = {
                "Content-Type": "image/png",
                "Cache-Control": "no-cache, no-store, must-revalidate",
                "Pragma": "no-cache",
                "Expires": "0",
                "Content-Disposition": "inline; filename=frame.png",
            }
            return web.Response(status=200, body=data, headers=headers)
        except Exception as e:
            logger.exception("/frame: PNG encode failed: %s", e)
            return web.json_response({"error": f"PNG encode failed: {e}"}, status=500)

    async def handle_health(self, request: web.Request) -> web.Response:
        sessions = {
            sid: getattr(pc, "connectionState", None)
            for sid, pc in self.sessions.items()
        }
        try:
            sources = self.ndi_cache.get_names() or list_source_names(timeout_ms=1000)
        except Exception:
            sources = []
        data = {
            "status": "ok",
            "sessions": {"count": len(self.sessions), "states": sessions},
            "ndi": {"selected": self.ndi_source, "selected_exact": self.ndi_exact_name, "selected_url": self.ndi_source_url, "available": sources},
        }
        return web.json_response(data)

    async def _cleanup(self, session_id: str):
        pc = self.sessions.pop(session_id, None)
        if pc:
            logger.info("WHEP session %s: closing", session_id)
            await pc.close()
        t = self._stat_tasks.pop(session_id, None)
        if t:
            t.cancel()

    async def on_shutdown(self, app: web.Application):
        tasks = [self._cleanup(sid) for sid in list(self.sessions.keys())]
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)
        # Cancel stat tasks if any linger
        for t in list(self._stat_tasks.values()):
            t.cancel()
        self._stat_tasks.clear()
        if self._cpu_task:
            self._cpu_task.cancel()
            self._cpu_task = None

    async def _log_sender_stats(self, session_id: str, sender: RTCRtpSender):
        prev = {}
        try:
            while True:
                await asyncio.sleep(2.0)
                try:
                    stats = await sender.getStats()
                except Exception as e:
                    logger.debug("stats error: %s", e)
                    continue
                for s in stats.values():
                    if getattr(s, 'type', None) == 'outbound-rtp' and getattr(s, 'kind', 'video') == 'video':
                        frames = getattr(s, 'framesEncoded', None)
                        bytes_sent = getattr(s, 'bytesSent', None)
                        total_enc = getattr(s, 'totalEncodeTime', None)  # seconds
                        ts = getattr(s, 'timestamp', None)
                        key = 'video'
                        if key in prev:
                            p = prev[key]
                            dt = (ts - p['ts']) / 1000.0 if ts and p['ts'] else 2.0
                            d_frames = (frames - p['frames']) if frames is not None and p['frames'] is not None else None
                            d_bytes = (bytes_sent - p['bytes']) if bytes_sent is not None and p['bytes'] is not None else None
                            d_enc = (total_enc - p['enc']) if total_enc is not None and p['enc'] is not None else None
                            fps = (d_frames / dt) if d_frames is not None and dt > 0 else None
                            mbps = (d_bytes * 8 / 1e6 / dt) if d_bytes is not None and dt > 0 else None
                            enc_ms = (1000.0 * d_enc / max(1, d_frames)) if d_enc is not None and d_frames and d_frames > 0 else None
                            logger.info(
                                "STATS %s: fps=%s bitrate=%.2f Mb/s enc=%.2f ms/frame frames=%s bytes=%s",
                                session_id,
                                f"{fps:.1f}" if fps is not None else "-",
                                mbps or 0.0,
                                enc_ms or 0.0,
                                d_frames if d_frames is not None else "-",
                                d_bytes if d_bytes is not None else "-",
                            )
                        prev[key] = {'frames': frames, 'bytes': bytes_sent, 'enc': total_enc, 'ts': ts}
                        break
        except asyncio.CancelledError:
            pass

    async def _log_process_cpu(self):
        import time as _t
        import os as _os
        last_wall = _t.perf_counter()
        last_cpu = _t.process_time()
        while True:
            await asyncio.sleep(2.0)
            wall = _t.perf_counter()
            cpu = _t.process_time()
            dw = wall - last_wall
            dc = cpu - last_cpu
            last_wall, last_cpu = wall, cpu
            if dw > 0:
                pct = 100.0 * dc / dw
                logger.info("PROC: cpu=%.1f%% cores=%s", pct, _os.cpu_count())


@web.middleware
async def cors_middleware(request: web.Request, handler):
    if request.method == "OPTIONS":
        return await handle_options(request)
    resp: web.StreamResponse = await handler(request)
    resp.headers["Access-Control-Allow-Origin"] = request.headers.get("Origin", "*")
    resp.headers["Access-Control-Allow-Methods"] = "GET, POST, PATCH, DELETE, OPTIONS"
    resp.headers["Access-Control-Allow-Headers"] = "Content-Type, Authorization"
    return resp


async def handle_options(request: web.Request) -> web.Response:
    headers = {
        "Access-Control-Allow-Origin": request.headers.get("Origin", "*"),
        "Access-Control-Allow-Methods": "GET, POST, PATCH, DELETE, OPTIONS",
        "Access-Control-Allow-Headers": "Content-Type, Authorization",
        "Access-Control-Max-Age": "86400",
    }
    return web.Response(status=204, headers=headers)


def create_web_app() -> web.Application:
    server = WhepServer()
    app = web.Application(middlewares=[cors_middleware])
    # Rebuild routes with named route for PATCH to compute Location
    app.router.add_get("/", server.handle_index)
    app.router.add_get("/ndi/sources", server.handle_ndi_sources)
    app.router.add_get("/ndi/sources/detail", server.handle_ndi_sources_detail)
    app.router.add_get("/frame", server.handle_frame_png)
    app.router.add_post("/ndi/select", server.handle_ndi_select)
    app.router.add_post("/ndi/select_url", server.handle_ndi_select_url)
    app.router.add_get("/health", server.handle_health)
    app.router.add_get("/healt", server.handle_health)
    app.router.add_get("/frame", server.handle_frame_png)
    app.router.add_post("/whep", server.handle_whep_post)
    app.router.add_options("/whep", handle_options)
    app.router.add_patch("/whep/{session_id}", server.handle_whep_patch, name="whep_patch")
    app.router.add_options("/whep/{session_id}", handle_options)
    app.router.add_delete("/whep/{session_id}", server.handle_whep_delete)
    # Static web player at /player
    root = Path(__file__).resolve().parent.parent
    static_dir = root / "web"
    if static_dir.exists():
        app.router.add_static("/player", path=str(static_dir), show_index=True)
    app.on_startup.append(server.on_startup)
    app.on_shutdown.append(server.on_shutdown)
    # Ensure cache stop on shutdown
    async def _stop_cache(app: web.Application):
        try:
            server.ndi_cache.stop()
        except Exception:
            pass
    app.on_shutdown.append(_stop_cache)
    return app


_INDEX_HTML = """
<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <title>WHEP Test Player</title>
    <style>
      body { font-family: system-ui, sans-serif; margin: 2rem; }
      video { width: 80vw; max-width: 1280px; background: #000; }
      .row { margin-top: 1rem; }
      input[type=text] { width: 40rem; }
      code { background: #f5f5f5; padding: 0.2rem 0.4rem; }
    </style>
  </head>
  <body>
    <h1>WHEP Test Player</h1>
    <div class="row">
      Endpoint: <input id="endpoint" type="text" value="/whep" />
      <button id="play">Play</button>
      <button id="stop" disabled>Stop</button>
    </div>
    <div class="row" style="position:relative; display:inline-block;">
      <video id="v" playsinline autoplay muted></video>
      <div id="hud" style="position:absolute;left:.5rem;top:.5rem;background:rgba(0,0,0,.5);color:#fff;padding:.2rem .4rem;border-radius:4px;font:12px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace">FPS: --</div>
    </div>
    <div class="row"><pre id="log"></pre></div>
    <script>
      const log = (msg) => { document.getElementById('log').textContent += msg + "\n"; };
      let pc = null; let resource = null;
      const playBtn = document.getElementById('play');
      const stopBtn = document.getElementById('stop');
      const hud = document.getElementById('hud');
      playBtn.onclick = async () => {
        const endpoint = document.getElementById('endpoint').value;
        pc = new RTCPeerConnection();
        pc.ontrack = (ev) => { document.getElementById('v').srcObject = ev.streams[0]; };
        const offer = await pc.createOffer({ offerToReceiveVideo: true, offerToReceiveAudio: false });
        await pc.setLocalDescription(offer);
        const resp = await fetch(endpoint, { method: 'POST', headers: { 'Content-Type': 'application/sdp' }, body: offer.sdp });
        if (!resp.ok) { log('POST failed: ' + resp.status); return; }
        resource = resp.headers.get('Location');
        const answerSdp = await resp.text();
        await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });
        // Start FPS HUD if supported
        const video = document.getElementById('v');
        if ('requestVideoFrameCallback' in HTMLVideoElement.prototype){
          let last = performance.now();
          let frames = 0;
          const loop = (now, meta)=>{
            frames++;
            if (now - last >= 1000){
              const fps = (frames * 1000 / (now - last)).toFixed(1);
              hud.textContent = `FPS: ${fps}`;
              frames = 0; last = now;
            }
            if (video.srcObject) video.requestVideoFrameCallback(loop);
          };
          video.requestVideoFrameCallback(loop);
        }
        playBtn.disabled = true; stopBtn.disabled = false;
      };
      stopBtn.onclick = async () => {
        if (resource) { try { await fetch(resource, { method: 'DELETE' }); } catch (e) {} }
        if (pc) { pc.close(); pc = null; }
        playBtn.disabled = false; stopBtn.disabled = true;
      };
    </script>
  </body>
  </html>
"""
