from __future__ import annotations

import importlib.util
import struct
import zipfile
from pathlib import Path

import pytest


MODULE_PATH = Path(__file__).parents[1] / "modelica" / "fmu_package.py"
SPEC = importlib.util.spec_from_file_location("fmu_package", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
fmu_package = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fmu_package)


def elf_header(machine: int, elf_class: int) -> bytes:
    header = bytearray(20)
    header[:4] = b"\x7fELF"
    header[4] = elf_class
    header[5] = 1
    struct.pack_into("<H", header, 18, machine)
    return bytes(header)


def write_fmu(path: Path, platform: str, binary: bytes) -> None:
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr("modelDescription.xml", "<fmiModelDescription/>")
        archive.writestr(f"binaries/{platform}/model.so", binary)


def test_fmi2_linux64_requires_x86_64(tmp_path: Path) -> None:
    valid = tmp_path / "valid.fmu"
    write_fmu(valid, "linux64", elf_header(62, 2))
    fmu_package.validate_fmu_package(valid)

    mislabeled = tmp_path / "mislabeled.fmu"
    write_fmu(mislabeled, "linux64", elf_header(183, 2))
    with pytest.raises(
        fmu_package.FMUPackageError,
        match="linux64 promises x86_64",
    ):
        fmu_package.validate_fmu_package(mislabeled)


def test_fmi3_aarch64_linux_accepts_arm64(tmp_path: Path) -> None:
    path = tmp_path / "arm64.fmu"
    write_fmu(path, "aarch64-linux", elf_header(183, 2))
    fmu_package.validate_fmu_package(path)


def test_fmu_rejects_unsafe_archive_member(tmp_path: Path) -> None:
    path = tmp_path / "unsafe.fmu"
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr("modelDescription.xml", "<fmiModelDescription/>")
        archive.writestr("../escape", "bad")
    with pytest.raises(fmu_package.FMUPackageError, match="unsafe"):
        fmu_package.validate_fmu_package(path)
