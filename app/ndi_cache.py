import asyncio
import logging
import time
from ctypes import byref, c_uint32
from typing import Dict, List, Optional, Tuple

from .ndi_ctypes import NDI, NDIlib_source_t

logger = logging.getLogger(__name__)


class NDINameCache:
    def __init__(self, refresh_ms: int = 1000):
        self.refresh_ms = max(100, int(refresh_ms))
        self._task: Optional[asyncio.Task] = None
        self._running = False
        self._names: List[str] = []
        self._map: Dict[str, Optional[str]] = {}
        self._ts: float = 0.0

    def start(self) -> None:
        if self._task is None:
            self._running = True
            self._task = asyncio.create_task(self._run())

    def stop(self) -> None:
        self._running = False
        if self._task:
            self._task.cancel()
            self._task = None

    def get_names(self) -> List[str]:
        return list(self._names)

    def get_details(self) -> List[Dict[str, Optional[str]]]:
        return [{"name": n, "url": self._map.get(n)} for n in self._names]

    def resolve_exact(self, name: str) -> Optional[str]:
        return self._map.get(name)

    def resolve_substring(self, query: str) -> Optional[Tuple[str, Optional[str]]]:
        q = (query or "").lower()
        for n in self._names:
            if q in n.lower():
                return n, self._map.get(n)
        return None

    async def _run(self):
        try:
            ndi = NDI()
        except Exception as e:
            logger.warning("NDINameCache: NDI init failed: %s", e)
            return
        # Keep a persistent finder instance so discovery can accumulate
        lib = ndi.lib
        inst = None
        try:
            inst = lib.NDIlib_find_create_v2(None)
            if not inst:
                logger.warning("NDINameCache: finder create returned null")
                return
            logger.info("NDINameCache: started (refresh=%dms)", self.refresh_ms)
            while self._running:
                try:
                    lib.NDIlib_find_wait_for_sources(inst, c_uint32(min(max(100, self.refresh_ms), 1500)))
                    count = c_uint32(0)
                    arr_ptr = lib.NDIlib_find_get_current_sources(inst, byref(count))
                    names: List[str] = []
                    mapping: Dict[str, Optional[str]] = {}
                    n = int(count.value)
                    for i in range(n):
                        s: NDIlib_source_t = arr_ptr[i]
                        name = (s.p_ndi_name or b"").decode("utf-8", errors="ignore") if s.p_ndi_name else None
                        url = (s.p_url_address or b"").decode("utf-8", errors="ignore") if s.p_url_address else None
                        if name:
                            names.append(name)
                            mapping[name] = url
                    self._names = names
                    self._map = mapping
                    self._ts = time.time()
                    logger.debug("NDINameCache: %d source(s) cached", len(names))
                except Exception as e:
                    logger.debug("NDINameCache: discovery error: %s", e)
                await asyncio.sleep(self.refresh_ms / 1000.0)
        except asyncio.CancelledError:
            pass
        finally:
            if inst:
                try:
                    lib.NDIlib_find_destroy(inst)
                except Exception:
                    pass
            logger.info("NDINameCache: stopped")
