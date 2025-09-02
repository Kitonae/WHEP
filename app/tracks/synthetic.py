import asyncio
import math
import time

import numpy as np
from aiortc import MediaStreamTrack
from av import VideoFrame


class SyntheticVideoTrack(MediaStreamTrack):
    kind = "video"

    def __init__(self, width: int = 1280, height: int = 720, fps: int = 30):
        super().__init__()
        self.width = width
        self.height = height
        self.fps = fps
        self._start = time.time()
        self._seq = 0
        self._frame_interval = 1 / fps

    async def recv(self) -> VideoFrame:
        # pace frames according to desired FPS
        target_time = self._start + self._seq * self._frame_interval
        now = time.time()
        wait = target_time - now
        if wait > 0:
            await asyncio.sleep(wait)
        self._seq += 1

        t = time.time() - self._start
        frame = self._make_pattern(t)
        # Allow PyAV to assign timing automatically
        return frame

    def _make_pattern(self, t: float) -> VideoFrame:
        # moving gradient bars with timestamp
        w, h = self.width, self.height
        x = np.linspace(0, 1, w, dtype=np.float32)
        y = np.linspace(0, 1, h, dtype=np.float32)[:, None]
        phase = (t * 0.25) % 1.0
        r = (x + phase) % 1.0
        g = (y + phase) % 1.0
        b = (0.5 + 0.5 * np.sin(2 * math.pi * (x + y + phase))).astype(np.float32)
        img = np.stack([
            (r * 255).astype(np.uint8).repeat(h).reshape(h, w),
            (g * 255).astype(np.uint8).repeat(w).reshape(h, w),
            (b * 255).astype(np.uint8),
        ], axis=2)
        frame = VideoFrame.from_ndarray(img, format="rgb24")
        frame = frame.reformat(width=w, height=h)
        return frame


class SilenceAudioTrack(MediaStreamTrack):
    kind = "audio"

    def __init__(self, sample_rate: int = 48000):
        super().__init__()
        self.sample_rate = sample_rate
        self._ts = 0
        self._samples_per_frame = int(sample_rate / 50)  # 20ms

    async def recv(self):
        await asyncio.sleep(0.02)
        samples = np.zeros((self._samples_per_frame, 1), dtype=np.int16)
        frame = VideoFrame()  # placeholder to keep typing simple; we will not use audio for now
        # aiortc expects av.AudioFrame, but to avoid av import complexity here, we omit audio track usage by default
        raise NotImplementedError("SilenceAudioTrack not wired; use video-only for now.")

