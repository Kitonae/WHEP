import ctypes
import ctypes.util
import os
import sys
from ctypes import byref, c_bool, c_char_p, c_float, c_int, c_int64, c_uint32, c_void_p, POINTER
from typing import List, Optional, Dict


class NDIError(RuntimeError):
    pass


def _load_library():
    # Prefer the DLL placed at repo root, else try system lookup.
    candidates: List[str] = []
    root = os.path.abspath(os.path.join(os.path.dirname(__file__), os.pardir))
    dll_name = "Processing.NDI.Lib.x64.dll"
    if sys.platform.startswith("win"):
        candidates.append(os.path.join(root, dll_name))
        # Add plain name to defer to system PATH
        candidates.append(dll_name)
    else:
        # Linux/mac fallback library names if present on the system
        for name in ("ndi", "ndi5", "ndi-v5", "Processing.NDI.Lib.x64"):
            p = ctypes.util.find_library(name)
            if p:
                candidates.append(p)
    last_err: Optional[BaseException] = None
    for path in candidates:
        try:
            if sys.platform.startswith("win"):
                return ctypes.WinDLL(path)
            else:
                return ctypes.CDLL(path)
        except OSError as e:
            last_err = e
    raise NDIError(f"Could not load NDI library from candidates: {candidates} ({last_err})")


# Struct and enum definitions (minimal subset required for discovery + receive)


class NDIlib_source_t(ctypes.Structure):
    _fields_ = [
        ("p_ndi_name", c_char_p),
        ("p_url_address", c_char_p),
    ]


class NDIlib_find_create_t(ctypes.Structure):
    _fields_ = [
        ("p_groups", c_char_p),
        ("show_local_sources", c_bool),
        ("p_extra_ips", c_char_p),
    ]


class NDIlib_video_frame_v2_t(ctypes.Structure):
    _fields_ = [
        ("xres", c_int),
        ("yres", c_int),
        ("FourCC", c_int),
        ("frame_rate_N", c_int),
        ("frame_rate_D", c_int),
        ("picture_aspect_ratio", c_float),
        ("frame_format_type", c_int),
        ("timecode", c_int64),
        ("p_data", ctypes.POINTER(ctypes.c_uint8)),
        ("line_stride_in_bytes", c_int),
        ("p_metadata", c_char_p),
        ("timestamp", c_int64),
    ]


class NDIlib_recv_create_v3_t(ctypes.Structure):
    _fields_ = [
        ("source_to_connect_to", NDIlib_source_t),
        ("color_format", c_int),
        ("bandwidth", c_int),
        ("allow_video_fields", c_bool),
        ("p_ndi_recv_name", c_char_p),
    ]


# Enums (values based on NDI SDK headers)
NDIlib_recv_color_format_fastest = 0
NDIlib_recv_color_format_UYVY_BGRA = 1
NDIlib_recv_color_format_BGRX_BGRA = 2
NDIlib_recv_color_format_RGBX_RGBA = 3
NDIlib_recv_color_format_best = 4
NDIlib_recv_bandwidth_highest = 2

NDIlib_frame_type_none = 0
NDIlib_frame_type_video = 1
NDIlib_frame_type_audio = 2
NDIlib_frame_type_metadata = 3
NDIlib_frame_type_error = 4
NDIlib_frame_type_status_change = 5


class NDI:
    def __init__(self) -> None:
        self.lib = _load_library()
        self._bind()
        if not self.lib.NDIlib_initialize():
            raise NDIError("NDIlib_initialize() failed")

    def _bind(self) -> None:
        l = self.lib
        # bool NDIlib_initialize(void)
        l.NDIlib_initialize.restype = c_bool

        # Finders
        # void* NDIlib_find_create_v2(const NDIlib_find_create_t*)
        l.NDIlib_find_create_v2.argtypes = [POINTER(NDIlib_find_create_t)]
        l.NDIlib_find_create_v2.restype = c_void_p
        # void NDIlib_find_destroy(void*)
        l.NDIlib_find_destroy.argtypes = [c_void_p]
        l.NDIlib_find_destroy.restype = None
        # const NDIlib_source_t* NDIlib_find_get_current_sources(void*, uint32_t* no_sources)
        l.NDIlib_find_get_current_sources.argtypes = [c_void_p, POINTER(c_uint32)]
        l.NDIlib_find_get_current_sources.restype = POINTER(NDIlib_source_t)
        # bool NDIlib_find_wait_for_sources(void*, uint32_t timeout_ms)
        l.NDIlib_find_wait_for_sources.argtypes = [c_void_p, c_uint32]
        l.NDIlib_find_wait_for_sources.restype = c_bool

        # Receiver
        # void* NDIlib_recv_create_v3(const NDIlib_recv_create_v3_t*)
        l.NDIlib_recv_create_v3.argtypes = [POINTER(NDIlib_recv_create_v3_t)]
        l.NDIlib_recv_create_v3.restype = c_void_p
        # void NDIlib_recv_destroy(void*)
        l.NDIlib_recv_destroy.argtypes = [c_void_p]
        l.NDIlib_recv_destroy.restype = None
        # int NDIlib_recv_capture_v2(void*, NDIlib_video_frame_v2_t*, void*, void*, uint32_t timeout)
        l.NDIlib_recv_capture_v2.argtypes = [c_void_p, POINTER(NDIlib_video_frame_v2_t), c_void_p, c_void_p, c_uint32]
        l.NDIlib_recv_capture_v2.restype = c_int
        # void NDIlib_recv_free_video_v2(void*, NDIlib_video_frame_v2_t*)
        l.NDIlib_recv_free_video_v2.argtypes = [c_void_p, POINTER(NDIlib_video_frame_v2_t)]
        l.NDIlib_recv_free_video_v2.restype = None

    # Discovery API
    def find_sources(self, timeout_ms: int = 1000,
                     groups: Optional[str] = None,
                     show_local: Optional[bool] = None,
                     extra_ips: Optional[str] = None) -> List[NDIlib_source_t]:
        # Always pass NULL for config to avoid ABI/struct layout mismatches across NDI versions (e.g., 6.2).
        # Advanced filters are not applied at the finder; callers can post-filter names.
        inst = self.lib.NDIlib_find_create_v2(None)
        if not inst:
            raise NDIError("NDIlib_find_create_v2() returned null")
        try:
            # wait for at least one update
            self.lib.NDIlib_find_wait_for_sources(inst, c_uint32(timeout_ms))
            count = c_uint32(0)
            arr_ptr = self.lib.NDIlib_find_get_current_sources(inst, byref(count))
            out: List[NDIlib_source_t] = []
            n = int(count.value)
            for i in range(n):
                out.append(arr_ptr[i])
            return out
        finally:
            self.lib.NDIlib_find_destroy(inst)

    def find_source_names(self, timeout_ms: int = 1000,
                           groups: Optional[str] = None,
                           show_local: Optional[bool] = None,
                           extra_ips: Optional[str] = None) -> List[str]:
        srcs = self.find_sources(timeout_ms=timeout_ms, groups=groups, show_local=show_local, extra_ips=extra_ips)
        names: List[str] = []
        for s in srcs:
            p = s.p_ndi_name
            if p:
                names.append(p.decode('utf-8', errors='ignore'))
        return names

    def find_source_names_budget(self, total_timeout_ms: int = 3000, poll_ms: int = 500,
                                 groups: Optional[str] = None,
                                 show_local: Optional[bool] = None,
                                 extra_ips: Optional[str] = None) -> List[str]:
        # Always pass NULL for config for cross-version safety; names can be filtered by caller.
        inst = self.lib.NDIlib_find_create_v2(None)
        if not inst:
            raise NDIError("NDIlib_find_create_v2() returned null")
        try:
            remaining = max(0, int(total_timeout_ms))
            step = max(1, int(poll_ms))
            names: List[str] = []
            while remaining >= 0:
                self.lib.NDIlib_find_wait_for_sources(inst, c_uint32(min(remaining, step)))
                count = c_uint32(0)
                arr_ptr = self.lib.NDIlib_find_get_current_sources(inst, byref(count))
                names = []
                for i in range(int(count.value)):
                    p = arr_ptr[i].p_ndi_name
                    if p:
                        names.append(p.decode('utf-8', errors='ignore'))
                remaining -= step
                if remaining < 0:
                    break
            return names
        finally:
            self.lib.NDIlib_find_destroy(inst)

    def recv_create_from_name(self, match_substring: str, timeout_ms: int = 2000) -> c_void_p:
        inst = self.lib.NDIlib_find_create_v2(None)
        if not inst:
            raise NDIError("NDIlib_find_create_v2() returned null")
        try:
            self.lib.NDIlib_find_wait_for_sources(inst, c_uint32(timeout_ms))
            count = c_uint32(0)
            arr_ptr = self.lib.NDIlib_find_get_current_sources(inst, byref(count))
            wanted = (match_substring or "").lower()
            chosen: Optional[NDIlib_source_t] = None
            for i in range(int(count.value)):
                s = arr_ptr[i]
                name = (s.p_ndi_name or b"").decode("utf-8", errors="ignore")
                if wanted in name.lower():
                    chosen = s
                    break
            if chosen is None:
                avail = []
                for i in range(int(count.value)):
                    p = arr_ptr[i].p_ndi_name
                    if p:
                        avail.append(p.decode("utf-8", errors="ignore"))
                raise NDIError(f"NDI source not found: '{match_substring}'. Available: {avail}")
            # Create receiver immediately while pointers are valid
            return self.recv_create(chosen)
        finally:
            self.lib.NDIlib_find_destroy(inst)

    def find_source_details_once(self, timeout_ms: int = 1000) -> List[Dict[str, Optional[str]]]:
        inst = self.lib.NDIlib_find_create_v2(None)
        if not inst:
            raise NDIError("NDIlib_find_create_v2() returned null")
        try:
            self.lib.NDIlib_find_wait_for_sources(inst, c_uint32(timeout_ms))
            count = c_uint32(0)
            arr_ptr = self.lib.NDIlib_find_get_current_sources(inst, byref(count))
            out: List[Dict[str, Optional[str]]] = []
            for i in range(int(count.value)):
                s = arr_ptr[i]
                name = (s.p_ndi_name or b"").decode('utf-8', errors='ignore') if s.p_ndi_name else None
                url = (s.p_url_address or b"").decode('utf-8', errors='ignore') if s.p_url_address else None
                out.append({"name": name, "url": url})
            return out
        finally:
            self.lib.NDIlib_find_destroy(inst)

    def recv_create_from_exact(self, exact_name: str, timeout_ms: int = 2000) -> c_void_p:
        inst = self.lib.NDIlib_find_create_v2(None)
        if not inst:
            raise NDIError("NDIlib_find_create_v2() returned null")
        try:
            self.lib.NDIlib_find_wait_for_sources(inst, c_uint32(timeout_ms))
            count = c_uint32(0)
            arr_ptr = self.lib.NDIlib_find_get_current_sources(inst, byref(count))
            chosen: Optional[NDIlib_source_t] = None
            for i in range(int(count.value)):
                s = arr_ptr[i]
                name = (s.p_ndi_name or b"").decode("utf-8", errors="ignore")
                if name == exact_name:
                    chosen = s
                    break
            if chosen is None:
                avail = []
                for i in range(int(count.value)):
                    p = arr_ptr[i].p_ndi_name
                    if p:
                        avail.append(p.decode("utf-8", errors="ignore"))
                raise NDIError(f"NDI source not found (exact): '{exact_name}'. Available: {avail}")
            return self.recv_create(chosen)
        finally:
            self.lib.NDIlib_find_destroy(inst)

    # Receiver API
    def recv_create(self, source: NDIlib_source_t) -> c_void_p:
        cfg = NDIlib_recv_create_v3_t()
        cfg.source_to_connect_to = source
        # Allow override of requested color format via env for compatibility across SDKs/senders.
        cfg.color_format = self._pick_color_format()
        cfg.bandwidth = NDIlib_recv_bandwidth_highest
        cfg.allow_video_fields = False
        cfg.p_ndi_recv_name = None
        h = self.lib.NDIlib_recv_create_v3(byref(cfg))
        if not h:
            raise NDIError("NDIlib_recv_create_v3() returned null")
        try:
            fmt_name = self._color_format_name(cfg.color_format)
        except Exception:
            fmt_name = str(cfg.color_format)
        # Basic print to help diagnose color pipeline
        print(f"NDI recv created with color_format={fmt_name}")
        return h

    def _pick_color_format(self) -> int:
        sel = (os.getenv("NDI_RECV_COLOR") or "").strip().upper()
        # Common aliases map to NDI enum values. If unknown, keep legacy default BGRX_BGRA.
        if sel in ("FAST", "FASTEST"):
            return NDIlib_recv_color_format_fastest
        if sel in ("BEST",):
            return NDIlib_recv_color_format_best
        if sel in ("UYVY", "UYVY_BGRA"):
            return NDIlib_recv_color_format_UYVY_BGRA
        if sel in ("BGRX", "BGRA", "BGRX_BGRA"):
            return NDIlib_recv_color_format_BGRX_BGRA
        if sel in ("RGBX", "RGBA", "RGBX_RGBA"):
            return NDIlib_recv_color_format_RGBX_RGBA
        # Default: prefer UYVY for broad compatibility and lower bandwidth
        return NDIlib_recv_color_format_UYVY_BGRA

    def _color_format_name(self, val: int) -> str:
        mapping = {
            NDIlib_recv_color_format_fastest: "FASTEST",
            NDIlib_recv_color_format_UYVY_BGRA: "UYVY",
            NDIlib_recv_color_format_BGRX_BGRA: "BGRX/BGRA",
            NDIlib_recv_color_format_RGBX_RGBA: "RGBX/RGBA",
            NDIlib_recv_color_format_best: "BEST",
        }
        return mapping.get(val, f"{val}")

    def recv_destroy(self, handle: c_void_p) -> None:
        if handle:
            self.lib.NDIlib_recv_destroy(handle)

    def recv_capture_video(self, handle: c_void_p, timeout_ms: int = 1000) -> Optional[NDIlib_video_frame_v2_t]:
        vf = NDIlib_video_frame_v2_t()
        ftype = self.lib.NDIlib_recv_capture_v2(handle, byref(vf), None, None, c_uint32(timeout_ms))
        if ftype == NDIlib_frame_type_video:
            return vf
        elif ftype in (NDIlib_frame_type_none, NDIlib_frame_type_status_change):
            return None
        elif ftype == NDIlib_frame_type_error:
            raise NDIError("NDI recv error")
        else:
            return None

    def recv_create_from_url(self, url: str) -> c_void_p:
        """Create a receiver for a specific NDI URL (e.g., 'ndi://...').
        This avoids name-based discovery and is stable across lists changing.
        """
        if not url:
            raise NDIError("Empty NDI URL")
        src = NDIlib_source_t()
        b_url = url.encode('utf-8')
        src.p_ndi_name = None
        src.p_url_address = c_char_p(b_url)
        return self.recv_create(src)

    def recv_free_video(self, handle: c_void_p, vf: NDIlib_video_frame_v2_t) -> None:
        self.lib.NDIlib_recv_free_video_v2(handle, byref(vf))


def list_source_names(timeout_ms: int = 2000,
                      groups: Optional[str] = None,
                      include_local: Optional[bool] = True,
                      extra_ips: Optional[str] = None) -> List[str]:
    ndi = NDI()
    return ndi.find_source_names_budget(total_timeout_ms=timeout_ms, poll_ms=500,
                                        groups=groups, show_local=include_local, extra_ips=extra_ips)

def list_source_details(timeout_ms: int = 1000) -> List[Dict[str, Optional[str]]]:
    ndi = NDI()
    return ndi.find_source_details_once(timeout_ms=timeout_ms)
