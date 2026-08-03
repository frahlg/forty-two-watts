from __future__ import annotations

import hashlib
import struct
from collections.abc import Iterable


class FingerprintWriter:
    """Typed SHA-256 input shared with Go.

    JSON text is not a stable cross-language hash input: Python and Go escape
    Unicode and HTML characters differently and do not print every float the
    same way. This format hashes UTF-8 strings and IEEE-754 values directly.
    """

    def __init__(self, domain: str) -> None:
        self._hash = hashlib.sha256()
        self.string(domain)

    def _uint64(self, value: int) -> None:
        self._hash.update(struct.pack(">Q", value))

    def string(self, value: str) -> None:
        data = value.encode("utf-8")
        self._hash.update(b"s")
        self._uint64(len(data))
        self._hash.update(data)

    def floating(self, value: float) -> None:
        self._hash.update(b"f")
        self._hash.update(struct.pack(">d", float(value)))

    def integer(self, value: int) -> None:
        self._hash.update(b"i")
        self._hash.update(struct.pack(">q", int(value)))

    def boolean(self, value: bool) -> None:
        self._hash.update(b"b\x01" if value else b"b\x00")

    def optional_float(self, value: float | None) -> None:
        self._hash.update(b"o\x00" if value is None else b"o\x01")
        if value is not None:
            self.floating(value)

    def strings(self, values: Iterable[str]) -> None:
        items = tuple(values)
        self._hash.update(b"l")
        self._uint64(len(items))
        for value in items:
            self.string(value)

    def hexdigest(self) -> str:
        return self._hash.hexdigest()
