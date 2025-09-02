import logging
import os
from typing import Optional

from aiortc import MediaStreamTrack

from .tracks.synthetic import SyntheticVideoTrack

logger = logging.getLogger(__name__)


class TrackBundle:
    def __init__(self, video: MediaStreamTrack, audio: Optional[MediaStreamTrack] = None):
        self.video = video
        self.audio = audio


def build_tracks(ndi_source: Optional[str] = None, ndi_url: Optional[str] = None, ndi_exact: Optional[str] = None) -> TrackBundle:
    """Choose NDI or synthetic track based on env vars.

    Env:
      NDI_SOURCE: name of the NDI source to receive from. If set, tries NDI.
      VIDEO_WIDTH / VIDEO_HEIGHT / FPS: overrides for synthetic.
    """
    source = ndi_source or os.getenv("NDI_SOURCE")
    if source:
        try:
            from .tracks.ndi_track import NdivideoTrack

            # By default, avoid in-track resizing for NDI; let the encoder scale via
            # RTCRtpSender encodings (VIDEO_SCALE_DOWN_BY). To force in-track resize,
            # set NDI_INTERNAL_RESIZE=1 and provide VIDEO_WIDTH/VIDEO_HEIGHT.
            width = height = None
            if (os.getenv("NDI_INTERNAL_RESIZE") or "").lower() in ("1", "true", "yes"):
                w_env = os.getenv("VIDEO_WIDTH")
                h_env = os.getenv("VIDEO_HEIGHT")
                width = int(w_env) if w_env else None
                height = int(h_env) if h_env else None
            video = NdivideoTrack(
                source,
                width=width,
                height=height,
                source_url=ndi_url or os.getenv("NDI_SOURCE_URL"),
                exact_name=ndi_exact,
            )
            logger.info("Configured NDI video source: %s%s%s",
                        source,
                        f" url={ndi_url or os.getenv('NDI_SOURCE_URL')}" if (ndi_url or os.getenv("NDI_SOURCE_URL")) else "",
                        f" exact='{ndi_exact}'" if ndi_exact else "")
            return TrackBundle(video=video)
        except Exception as e:
            logger.warning("Falling back to synthetic video: %s", e)

    width = int(os.getenv("VIDEO_WIDTH", "1280"))
    height = int(os.getenv("VIDEO_HEIGHT", "720"))
    fps = int(os.getenv("FPS", "30"))
    video = SyntheticVideoTrack(width=width, height=height, fps=fps)
    logger.info("Configured synthetic video %dx%d@%dfps", width, height, fps)
    return TrackBundle(video=video)
