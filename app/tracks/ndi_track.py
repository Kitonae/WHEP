import asyncio
import logging
import os
import threading
from typing import Optional, Tuple

import ctypes
import numpy as np
from aiortc import VideoStreamTrack
from av import VideoFrame

from ..ndi_ctypes import (
    NDI,
    NDIError,
    NDIlib_source_t,
)

logger = logging.getLogger(__name__)


class NDIUnavailable(Exception):
    pass


class NdivideoTrack(VideoStreamTrack):
    kind = "video"

    def __init__(self, source_name: str, width: Optional[int] = None, height: Optional[int] = None, source_url: Optional[str] = None, exact_name: Optional[str] = None):
        super().__init__()
        self.source_name = source_name
        self.source_url = source_url
        self.exact_name = exact_name
        self.width = width
        self.height = height
        self._ndi: Optional[NDI] = None
        self._receiver: Optional[int] = None  # c_void_p as int
        self._queue = asyncio.Queue(maxsize=2)
        self._task: Optional[asyncio.Task] = None
        self._thread: Optional[threading.Thread] = None
        self._running: bool = False
        self._loop: Optional[asyncio.AbstractEventLoop] = None
        # NDI receive capture timeout in ms (short to keep loop responsive)
        try:
            self._recv_timeout_ms = int(os.getenv("NDI_RECV_TIMEOUT_MS", "50"))
        except ValueError:
            self._recv_timeout_ms = 50
        # Optional output pixel format to pre-convert (e.g., 'yuv420p')
        self._output_pix_fmt: Optional[str] = os.getenv("NDI_OUTPUT_PIXFMT") or None

    async def start(self):
        if self._ndi is not None:
            return
        try:
            self._ndi = NDI()
        except Exception as e:
            logger.exception("Failed to initialize NDI: %s", e)
            raise NDIUnavailable(str(e))

        # Build receiver with robust fallback order: URL -> exact -> substring
        try:
            if self.source_url:
                logger.info("NDI: creating receiver by URL: %s", self.source_url)
                self._receiver = self._ndi.recv_create_from_url(self.source_url)
            elif self.exact_name:
                # First try to resolve to URL and connect by URL
                logger.debug("NDI: resolving exact name to URL: %s", self.exact_name)
                for _ in range(0, 10):  # ~5s total at 500ms per try
                    try:
                        srcs = self._ndi.find_sources(timeout_ms=500)
                        found_url = None
                        for s in srcs:
                            name = (s.p_ndi_name or b"").decode("utf-8", errors="ignore")
                            if name == self.exact_name:
                                found_url = (s.p_url_address or b"").decode("utf-8", errors="ignore")
                                break
                        if found_url:
                            logger.info("NDI: resolved exact name to URL: %s", found_url)
                            self._receiver = self._ndi.recv_create_from_url(found_url)
                            break
                    except Exception:
                        pass
                if self._receiver is None:
                    # Fallback to substring match using provided name
                    logger.info("NDI: falling back to substring match: %s", self.source_name or self.exact_name)
                    self._receiver = self._ndi.recv_create_from_name(self.source_name or self.exact_name, timeout_ms=2000)
            else:
                logger.info("NDI: creating receiver by substring match: %s", self.source_name)
                self._receiver = self._ndi.recv_create_from_name(self.source_name, timeout_ms=2000)
        except NDIError as e:
            logger.exception("Failed to create NDI receiver: %s", e)
            raise NDIUnavailable(str(e))

        # Start capture worker thread
        self._loop = asyncio.get_running_loop()
        self._running = True
        self._thread = threading.Thread(target=self._capture_loop, name="ndi-capture", daemon=True)
        self._thread.start()

    def _capture_loop(self):
        assert self._ndi is not None and self._receiver is not None
        logger.info("NDI pull loop started for %s", self.source_name)

        def push_frame(f: VideoFrame):
            try:
                self._queue.put_nowait(f)
            except asyncio.QueueFull:
                try:
                    _ = self._queue.get_nowait()
                except Exception:
                    pass
                try:
                    self._queue.put_nowait(f)
                except Exception:
                    pass

        n = 0
        try:
            while self._running:
                vf = self._ndi.recv_capture_video(self._receiver, timeout_ms=max(1, self._recv_timeout_ms))
                if vf is None:
                    continue
                try:
                    w, h, stride = int(vf.xres), int(vf.yres), int(vf.line_stride_in_bytes)
                    buf_len = stride * h
                    data_ptr = ctypes.cast(vf.p_data, ctypes.POINTER(ctypes.c_ubyte))
                    flat = np.ctypeslib.as_array(data_ptr, shape=(buf_len,))
                    row_view = flat.reshape(h, stride)

                    # Detect incoming pixel format using FourCC and convert appropriately.
                    fourcc = int(getattr(vf, "FourCC", 0))
                    try:
                        fourcc_str = fourcc.to_bytes(4, byteorder="little").decode("ascii", errors="ignore")
                    except Exception:
                        fourcc_str = ""
                    if n == 0:
                        logger.info("NDI: first frame FourCC='%s' (%d), stride=%d, size=%dx%d", fourcc_str, fourcc, stride, w, h)

                    if fourcc_str in ("BGRA", "BGRX", "RGBA", "RGBX", "ARGB", "ABGR"):
                        # 4 bytes per pixel path
                        trimmed = row_view[:, : w * 4].copy()
                        frame_arr = trimmed.reshape(h, w, 4)
                        # Allow override for ambiguous vendor outputs
                        force_order = os.getenv("NDI_INPUT_RGBA_ORDER", "").upper().strip()
                        if force_order in ("RGBA", "BGRA", "ARGB", "ABGR", "RGBX", "BGRX", "RGB0", "BGR0"):
                            order = force_order
                        else:
                            order = fourcc_str or "RGBA"
                        # Map order to PyAV pixel formats
                        order_map = {
                            "RGBA": "rgba",
                            "BGRA": "bgra",
                            "ARGB": "argb",
                            "ABGR": "abgr",
                            "RGBX": "rgb0",  # ignore alpha
                            "BGRX": "bgr0",  # ignore alpha
                            "RGB0": "rgb0",
                            "BGR0": "bgr0",
                        }
                        pixfmt = order_map.get(order, "rgba")
                        frame = VideoFrame.from_ndarray(frame_arr, format=pixfmt)
                    elif fourcc_str in ("UYVY", "2vuy", "YUY2", "YUYV"):
                        # 4:2:2 packed YUV. Convert directly to I420 (yuv420p) to reduce encoder work.
                        Y, U420, V420 = self._uyvy_to_i420_planes(row_view[:, : w * 2], w, h, fourcc_str)
                        frame = self._frame_from_i420_planes(Y, U420, V420)
                    else:
                        # If stride matches 2 bytes per pixel, treat as YUV422 (default UYVY unless overridden).
                        if stride >= w * 2 and stride < w * 4:
                            order = os.getenv("NDI_INPUT_YUV422_ORDER", "UYVY").upper()
                            if order not in ("UYVY", "YUY2", "YUYV", "2VUY"):
                                order = "UYVY"
                            Y, U420, V420 = self._uyvy_to_i420_planes(row_view[:, : w * 2], w, h, order)
                            frame = self._frame_from_i420_planes(Y, U420, V420)
                            if n % 120 == 0:
                                logger.warning("NDI: unknown FourCC '%s' (%d); inferred 2bpp -> %s conversion", fourcc_str, fourcc, order)
                        else:
                            # Fallback: assume 4-byte BGRA to avoid crashes; log once per few seconds.
                            trimmed = row_view[:, : w * 4].copy()
                            frame_arr = trimmed.reshape(h, w, 4)
                            frame = VideoFrame.from_ndarray(frame_arr, format="bgra")
                            if n % 120 == 0:
                                logger.warning("NDI: unexpected FourCC '%s' (%d); assuming BGRA", fourcc_str, fourcc)
                    target_w = self.width or w
                    target_h = self.height or h
                    if target_w != w or target_h != h:
                        frame = frame.reformat(width=target_w, height=target_h)
                    if self._output_pix_fmt:
                        try:
                            frame = frame.reformat(format=self._output_pix_fmt)
                        except Exception as e:
                            logger.debug("NDI: pixfmt %s reformat failed: %s", self._output_pix_fmt, e)
                    # Hand over to event loop queue
                    if self._loop:
                        self._loop.call_soon_threadsafe(push_frame, frame)
                    n += 1
                    if n % 120 == 0:
                        logger.debug("NDI: captured %d frames (%dx%d)", n, frame.width, frame.height)
                except Exception as e:
                    logger.exception("NDI capture loop error: %s", e)
                finally:
                    try:
                        self._ndi.recv_free_video(self._receiver, vf)
                    except Exception:
                        pass
        finally:
            logger.info("NDI pull loop exited for %s", self.source_name)

    @staticmethod
    def _convert_yuv422_to_rgb24(rows: np.ndarray, w: int, h: int, fourcc: str) -> np.ndarray:
        """
        Convert packed YUV 4:2:2 (UYVY or YUY2) to RGB24.
        - rows: ndarray of shape (h, stride) with at least w*2 bytes per row.
        - fourcc: one of 'UYVY', '2vuy', 'YUY2', 'YUYV'.
        Returns ndarray of shape (h, w, 3) dtype=uint8.
        """
        # Ensure contiguous copy of active width
        active = rows[:, : w * 2]
        # Reshape to groups of 4 bytes per 2 pixels
        quad = active.reshape(h, w // 2, 4).astype(np.int16)

        if fourcc in ("UYVY", "2vuy"):
            U = quad[:, :, 0] - 128
            Y0 = quad[:, :, 1] - 16
            V = quad[:, :, 2] - 128
            Y1 = quad[:, :, 3] - 16
        else:  # YUY2 / YUYV
            Y0 = quad[:, :, 0] - 16
            U = quad[:, :, 1] - 128
            Y1 = quad[:, :, 2] - 16
            V = quad[:, :, 3] - 128

        # Optional UV swap to correct chroma order issues
        try:
            if os.getenv("NDI_SWAP_UV", "0") in ("1", "true", "True", "YES", "yes"):
                U, V = V, U
        except Exception:
            pass

        # Clamp Y to non-negative to avoid large negatives
        Y0 = np.clip(Y0, 0, None)
        Y1 = np.clip(Y1, 0, None)

        # Broadcast chroma to pixel-aligned arrays
        U_b = U.astype(np.int32)
        V_b = V.astype(np.int32)
        C0 = Y0.astype(np.int32)
        C1 = Y1.astype(np.int32)

        def yuv_to_rgb(C: np.ndarray, U: np.ndarray, V: np.ndarray) -> Tuple[np.ndarray, np.ndarray, np.ndarray]:
            # ITU-R BT.601 full-range-ish conversion (approx). Scale factors in integer domain.
            R = (298 * C + 409 * V + 128) >> 8
            G = (298 * C - 100 * U - 208 * V + 128) >> 8
            B = (298 * C + 516 * U + 128) >> 8
            R = np.clip(R, 0, 255).astype(np.uint8)
            G = np.clip(G, 0, 255).astype(np.uint8)
            B = np.clip(B, 0, 255).astype(np.uint8)
            return R, G, B

        R0, G0, B0 = yuv_to_rgb(C0, U_b, V_b)
        R1, G1, B1 = yuv_to_rgb(C1, U_b, V_b)

        # Interleave two pixels per group back to width
        R = np.empty((h, w), dtype=np.uint8)
        G = np.empty((h, w), dtype=np.uint8)
        B = np.empty((h, w), dtype=np.uint8)
        R[:, 0::2] = R0; R[:, 1::2] = R1
        G[:, 0::2] = G0; G[:, 1::2] = G1
        B[:, 0::2] = B0; B[:, 1::2] = B1

        rgb = np.dstack((R, G, B))
        return rgb

    @staticmethod
    def _uyvy_to_i420_planes(rows: np.ndarray, w: int, h: int, fourcc: str) -> Tuple[np.ndarray, np.ndarray, np.ndarray]:
        """Convert packed YUV422 (UYVY/YUY2) to planar I420 (Y, U, V).
        Returns three contiguous arrays:
          - Y: (h, w) uint8
          - U: (h//2, w//2) uint8
          - V: (h//2, w//2) uint8
        """
        active = rows[:, : w * 2]
        quad = active.reshape(h, w // 2, 4)
        if fourcc.upper() in ("UYVY", "2VUY"):
            U = quad[:, :, 0]
            Y0 = quad[:, :, 1]
            V = quad[:, :, 2]
            Y1 = quad[:, :, 3]
        else:  # YUY2 / YUYV
            Y0 = quad[:, :, 0]
            U = quad[:, :, 1]
            Y1 = quad[:, :, 2]
            V = quad[:, :, 3]
        # Build full-res Y
        Y = np.empty((h, w), dtype=np.uint8)
        Y[:, 0::2] = Y0
        Y[:, 1::2] = Y1
        # Downsample U and V vertically to 4:2:0 by averaging adjacent rows
        U_even = U[0:h:2, :].astype(np.uint16)
        U_odd = U[1:h:2, :].astype(np.uint16)
        if U_odd.shape[0] != U_even.shape[0]:
            U_odd = np.vstack([U_odd, U_odd[-1:]]) if U_odd.size else U_even
        V_even = V[0:h:2, :].astype(np.uint16)
        V_odd = V[1:h:2, :].astype(np.uint16)
        if V_odd.shape[0] != V_even.shape[0]:
            V_odd = np.vstack([V_odd, V_odd[-1:]]) if V_odd.size else V_even
        U420 = ((U_even + U_odd) // 2).astype(np.uint8)
        V420 = ((V_even + V_odd) // 2).astype(np.uint8)
        try:
            if os.getenv("NDI_SWAP_UV", "0") in ("1", "true", "True", "YES", "yes"):
                U420, V420 = V420, U420
        except Exception:
            pass
        return Y, U420, V420

    @staticmethod
    def _frame_from_i420_planes(Y: np.ndarray, U: np.ndarray, V: np.ndarray) -> VideoFrame:
        """Create a VideoFrame in format yuv420p from planar Y, U, V arrays.
        Handles stride differences by using plane.update().
        """
        h, w = Y.shape
        frame = VideoFrame(width=w, height=h, format="yuv420p")
        # Y plane
        p0 = frame.planes[0]
        if p0.line_size == w:
            p0.update(Y.tobytes())
        else:
            # Fallback: copy row by row if padded
            mv = memoryview(p0)
            for y in range(h):
                start = y * p0.line_size
                mv[start:start + w] = Y[y].tobytes()
        # U plane
        ph = h // 2
        pw = w // 2
        p1 = frame.planes[1]
        if p1.line_size == pw:
            p1.update(U.tobytes())
        else:
            mv = memoryview(p1)
            for y in range(ph):
                start = y * p1.line_size
                mv[start:start + pw] = U[y].tobytes()
        # V plane
        p2 = frame.planes[2]
        if p2.line_size == pw:
            p2.update(V.tobytes())
        else:
            mv = memoryview(p2)
            for y in range(ph):
                start = y * p2.line_size
                mv[start:start + pw] = V[y].tobytes()
        return frame

    async def recv(self) -> VideoFrame:
        if self._thread is None:
            await self.start()
        frame: VideoFrame = await self._queue.get()
        pts, time_base = await self.next_timestamp()
        frame.pts = pts
        frame.time_base = time_base
        return frame

    def stop(self) -> None:
        try:
            # Stop capture thread
            self._running = False
            if self._thread and self._thread.is_alive():
                logger.info("NDI: stopping capture thread for %s", self.source_name)
                self._thread.join(timeout=1.0)
        finally:
            if self._ndi and self._receiver:
                try:
                    logger.debug("NDI: destroying receiver")
                    self._ndi.recv_destroy(self._receiver)
                except Exception:
                    pass
            self._receiver = None
            self._ndi = None
            self._thread = None
