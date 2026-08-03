from __future__ import annotations

import stat
import struct
import zipfile
from pathlib import Path, PurePosixPath


MAX_ARCHIVE_ENTRIES = 10_000
MAX_UNCOMPRESSED_BYTES = 512 * 1024 * 1024


class FMUPackageError(ValueError):
    pass


_LINUX_PLATFORMS = {
    # FMI 2 legacy names map only to x86. FMI 3 adds explicit tuples.
    "linux32": (3, 1, "x86"),
    "linux64": (62, 2, "x86_64"),
    "x86-linux": (3, 1, "x86"),
    "x86_64-linux": (62, 2, "x86_64"),
    "aarch32-linux": (40, 1, "aarch32"),
    "aarch64-linux": (183, 2, "aarch64"),
}

_ELF_MACHINES = {
    3: "x86",
    40: "aarch32",
    62: "x86_64",
    183: "aarch64",
}


def _safe_member(info: zipfile.ZipInfo) -> bool:
    name = info.filename
    if not name or "\\" in name:
        return False
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts:
        return False
    mode = (info.external_attr >> 16) & 0o170000
    return mode != stat.S_IFLNK


def _elf_identity(header: bytes) -> tuple[int, int] | None:
    if len(header) < 20 or header[:4] != b"\x7fELF":
        return None
    elf_class = header[4]
    byte_order = header[5]
    if elf_class not in {1, 2} or byte_order not in {1, 2}:
        return None
    endian = "<" if byte_order == 1 else ">"
    machine = struct.unpack(f"{endian}H", header[18:20])[0]
    return machine, elf_class


def validate_fmu_package(path: Path) -> None:
    """Check archive safety and the architecture promised by Linux labels.

    This runs before an importer extracts or loads native code. It does not
    replace FMI schema or simulation checks.
    """

    try:
        archive = zipfile.ZipFile(path)
    except (OSError, zipfile.BadZipFile) as exc:
        raise FMUPackageError(f"cannot open FMU archive: {exc}") from exc

    with archive:
        infos = archive.infolist()
        if len(infos) > MAX_ARCHIVE_ENTRIES:
            raise FMUPackageError(
                f"FMU has {len(infos)} entries; limit is {MAX_ARCHIVE_ENTRIES}"
            )
        total_size = sum(info.file_size for info in infos)
        if total_size > MAX_UNCOMPRESSED_BYTES:
            raise FMUPackageError(
                "FMU uncompressed size exceeds the 512 MiB verification limit"
            )
        if "modelDescription.xml" not in {info.filename for info in infos}:
            raise FMUPackageError("FMU has no modelDescription.xml")

        for info in infos:
            if not _safe_member(info):
                raise FMUPackageError(
                    f"FMU contains unsafe archive member {info.filename!r}"
                )
            parts = PurePosixPath(info.filename).parts
            if (
                info.is_dir()
                or len(parts) < 3
                or parts[0] != "binaries"
                or not parts[-1].endswith(".so")
            ):
                continue
            platform = parts[1]
            expected = _LINUX_PLATFORMS.get(platform)
            if expected is None:
                continue
            with archive.open(info) as stream:
                identity = _elf_identity(stream.read(20))
            if identity is None:
                raise FMUPackageError(
                    f"{info.filename} is not a valid ELF shared library"
                )
            machine, elf_class = identity
            expected_machine, expected_class, expected_name = expected
            if machine != expected_machine or elf_class != expected_class:
                actual_name = _ELF_MACHINES.get(machine, f"ELF machine {machine}")
                raise FMUPackageError(
                    f"{info.filename} contains {actual_name} code, but "
                    f"{platform} promises {expected_name}"
                )
