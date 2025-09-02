import argparse
import asyncio
import logging
import os
from typing import Optional

from .logging_setup import setup_logging
from .media import build_tracks
from .whep_server import create_web_app


def parse_args():
    p = argparse.ArgumentParser(description="Generate synthetic (or NDI) video frames – no web server")
    p.add_argument("--ndi", dest="ndi_source", default=os.getenv("NDI_SOURCE"), help="NDI source name (optional)")
    p.add_argument("--fps", type=int, default=int(os.getenv("FPS", "30")), help="Synthetic FPS")
    p.add_argument("--width", type=int, default=int(os.getenv("VIDEO_WIDTH", "1280")), help="Synthetic width")
    p.add_argument("--height", type=int, default=int(os.getenv("VIDEO_HEIGHT", "720")), help="Synthetic height")
    p.add_argument("--mode", choices=["frames", "server"], default="frames", help="Operation mode: frames = generate frames; server = run WHEP endpoint")
    p.add_argument("--frames", type=int, default=60, help="(frames mode) Number of frames to generate (<=0 = run until Ctrl+C)")
    p.add_argument("--save-last-frame", dest="save_last", help="(frames mode) Path to save the final frame as raw RGB (.npy) file")
    p.add_argument("--host", default=os.getenv("HOST", "0.0.0.0"), help="(server mode) Bind host")
    p.add_argument("--port", type=int, default=int(os.getenv("PORT", "8000")), help="(server mode) Bind port")
    return p.parse_args()


async def _frame_loop(total_frames: int, save_last: Optional[str]):
    bundle = build_tracks()
    video = bundle.video
    log = logging.getLogger("framegen")
    count = 0
    last_frame = None
    try:
        while total_frames <= 0 or count < total_frames:
            frame = await video.recv()
            count += 1
            if count % 10 == 0:
                log.info("Generated %d frames (%sx%s)", count, frame.width, frame.height)
            last_frame = frame
    except KeyboardInterrupt:
        log.info("Interrupted after %d frames", count)
    if save_last and last_frame is not None:
        # store raw RGB24 ndarray using numpy (avoid extra image libs)
        import numpy as np

        arr = last_frame.to_ndarray(format="rgb24")
        np.save(save_last, arr)
        log.info("Saved last frame to %s (shape=%s)", save_last + ".npy", arr.shape)
    log.info("Done. Total frames: %d", count)


def main():
    setup_logging()
    args = parse_args()

    if args.ndi_source:
        os.environ["NDI_SOURCE"] = args.ndi_source
    os.environ["FPS"] = str(args.fps)
    os.environ["VIDEO_WIDTH"] = str(args.width)
    os.environ["VIDEO_HEIGHT"] = str(args.height)

    if args.mode == "frames":
        asyncio.run(_frame_loop(args.frames, args.save_last))
    else:
        from aiohttp import web

        app = create_web_app()
        web.run_app(app, host=args.host, port=args.port)


if __name__ == "__main__":
    main()

