import asyncio
import logging
from typing import Optional

import ctypes
import numpy as np
from aiortc import MediaStreamTrack
from av import VideoFrame

from ..ndi_ctypes import (
    NDI,
    NDIError,
    NDIlib_source_t,
)

logger = logging.getLogger(__name__)


class NDIUnavailable(Exception):
    pass


class NdivideoTrack(MediaStreamTrack):
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
        self._queue = asyncio.Queue(maxsize=1)
        self._task: Optional[asyncio.Task] = None

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
                self._receiver = self._ndi.recv_create_from_url(self.source_url)
            elif self.exact_name:
                # First try to resolve to URL and connect by URL
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
                            self._receiver = self._ndi.recv_create_from_url(found_url)
                            break
                    except Exception:
                        pass
                if self._receiver is None:
                    # Fallback to substring match using provided name
                    self._receiver = self._ndi.recv_create_from_name(self.source_name or self.exact_name, timeout_ms=2000)
            else:
                self._receiver = self._ndi.recv_create_from_name(self.source_name, timeout_ms=2000)
        except NDIError as e:
            logger.exception("Failed to create NDI receiver: %s", e)
            raise NDIUnavailable(str(e))

        self._task = asyncio.create_task(self._pull_loop())

    async def _pull_loop(self):
        assert self._ndi is not None and self._receiver is not None
        logger.info("NDI pull loop started for %s", self.source_name)

        def _capture_once():
            # Blocking call into NDI; returns a prepared VideoFrame or None
            vf = self._ndi.recv_capture_video(self._receiver, timeout_ms=1000)
            if vf is None:
                return None
            try:
                w, h, stride = int(vf.xres), int(vf.yres), int(vf.line_stride_in_bytes)
                buf_len = stride * h
                data_ptr = ctypes.cast(vf.p_data, ctypes.POINTER(ctypes.c_ubyte))
                flat = np.ctypeslib.as_array(data_ptr, shape=(buf_len,))
                row_view = flat.reshape(h, stride)
                trimmed = row_view[:, : w * 4].copy()
                frame_bgra = trimmed.reshape(h, w, 4)
                frame = VideoFrame.from_ndarray(frame_bgra, format="bgra")
                if self.width and self.height and (self.width != w or self.height != h):
                    frame = frame.reformat(width=self.width, height=self.height)
                return frame
            finally:
                # Always free the captured NDI frame in the same thread
                self._ndi.recv_free_video(self._receiver, vf)

        try:
            while True:
                frame = await asyncio.to_thread(_capture_once)
                if frame is None:
                    await asyncio.sleep(0)
                    continue
                # Try non-blocking put; drop if queue full to avoid lag buildup
                try:
                    self._queue.put_nowait(frame)
                except asyncio.QueueFull:
                    _ = self._queue.get_nowait()
                    self._queue.put_nowait(frame)
        except asyncio.CancelledError:
            pass
        finally:
            logger.info("NDI pull loop exited for %s", self.source_name)

    async def recv(self) -> VideoFrame:
        if self._task is None:
            await self.start()
        frame: VideoFrame = await self._queue.get()
        return frame

    def stop(self) -> None:
        try:
            if self._task:
                self._task.cancel()
        finally:
            if self._ndi and self._receiver:
                try:
                    self._ndi.recv_destroy(self._receiver)
                except Exception:
                    pass
            self._receiver = None
            self._ndi = None
