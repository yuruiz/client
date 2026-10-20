#!/usr/bin/env python3
"""Create an immutable source release and its Homebrew formula. Never uploads."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
from urllib.parse import urlparse


NAME = "git-remote-keybase-standalone"
ROOT = Path(__file__).resolve().parents[2]


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args])


def source_files(repo):
    paths = git(repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard",
                "--", "go", "LICENSE", "packaging/homebrew").split(b"\0")
    files = sorted({os.fsdecode(path) for path in paths if path})
    # Git can still list files deleted in the working tree.
    files = [name for name in files if (repo / name).exists() or (repo / name).is_symlink()]
    for required in ("go/go.mod", "go/go.sum", "LICENSE",
                     "go/kbfs/kbfsgit/git-remote-keybase-standalone/main.go"):
        if required not in files:
            raise ValueError(f"Missing release source: {required}")
    for name in files:
        path = repo / name
        if path.is_symlink() and Path(os.readlink(path)).is_absolute():
            raise ValueError(f"Source link must be relative: {name}")
        if not path.resolve().is_relative_to(repo.resolve()):
            raise ValueError(f"Source link leaves the repository: {name}")
    return files


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source_archive(repo, destination, version):
    files = source_files(repo)
    revision = git(repo, "rev-parse", "HEAD").decode().strip()
    epoch = int(git(repo, "show", "-s", "--format=%ct", "HEAD"))
    provenance = {
        "name": NAME, "version": version, "base_commit": revision,
        "working_tree_changes": bool(git(repo, "status", "--porcelain", "--", "go", "LICENSE", "packaging/homebrew")),
        "files": {name: sha256(repo / name) for name in files if not (repo / name).is_symlink()},
    }
    prefix = f"{NAME}-{version}"
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=destination.parent, prefix=".source-", delete=False) as temporary:
        temporary_path = Path(temporary.name)
    try:
        with temporary_path.open("wb") as output:
            with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=epoch) as zipped:
                with tarfile.open(fileobj=zipped, mode="w", format=tarfile.PAX_FORMAT) as archive:
                    for name in files:
                        path = repo / name
                        info = archive.gettarinfo(str(path), arcname=f"{prefix}/{name}")
                        info.uid = info.gid = 0
                        info.uname = info.gname = ""
                        info.mtime = epoch
                        info.pax_headers = {}
                        info.mode = 0o755 if path.stat().st_mode & 0o111 else 0o644
                        if info.isreg():
                            with path.open("rb") as source:
                                archive.addfile(info, source)
                        else:
                            archive.addfile(info)
                    metadata = json.dumps(provenance, sort_keys=True, indent=2).encode() + b"\n"
                    info = tarfile.TarInfo(f"{prefix}/SOURCE.json")
                    info.size, info.mtime, info.mode = len(metadata), epoch, 0o644
                    archive.addfile(info, io.BytesIO(metadata))
        digest = sha256(temporary_path)
        if destination.exists():
            if sha256(destination) != digest:
                raise ValueError(f"{destination} already contains a different source snapshot; use a new version")
        else:
            temporary_path.chmod(0o644)
            temporary_path.replace(destination)
        return digest
    finally:
        temporary_path.unlink(missing_ok=True)


def ruby_string(value):
    # JSON quoting handles escapes; also disable Ruby string interpolation.
    return json.dumps(value, ensure_ascii=True).replace("#", r"\#")


def render_formula(template, *, version, url, digest, homepage):
    if urlparse(url).scheme not in ("https", "file"):
        raise ValueError("Source URL must be https:// or a local file:// URL")
    if urlparse(homepage).scheme != "https":
        raise ValueError("Homepage must use HTTPS")
    result = template
    for key, value in {"VERSION": version, "SOURCE_URL": url, "SHA256": digest, "HOMEPAGE": homepage}.items():
        result = result.replace(f"@{key}@", ruby_string(value))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", default=(ROOT / "packaging/homebrew/VERSION").read_text().strip())
    parser.add_argument("--output", type=Path, default=Path.home() / ".local/share/keybase-git-standalone/releases")
    parser.add_argument("--source-url", help="Published source archive URL; defaults to the generated local file URL")
    parser.add_argument("--homepage", default="https://github.com/yuruiz/client/tree/standalone-git-helper",
                        help="Package homepage (default: standalone helper source branch)")
    args = parser.parse_args()
    if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
        parser.error("version must have the form MAJOR.MINOR.PATCH")
    output = args.output.expanduser().resolve() / args.version
    if any(output.is_relative_to(ROOT / part) for part in ("go", "packaging/homebrew")):
        parser.error("output must be outside the archived source directories")
    archive = output / f"{NAME}-{args.version}.tar.gz"
    digest = source_archive(ROOT, archive, args.version)
    source_url = args.source_url or archive.as_uri()
    formula = output / f"{NAME}.rb"
    template = (ROOT / f"packaging/homebrew/{NAME}.rb.in").read_text()
    formula.write_text(render_formula(template, version=args.version, url=source_url,
                                      digest=digest, homepage=args.homepage))
    (output / "SHA256SUMS").write_text(f"{digest}  {archive.name}\n")
    print(json.dumps({"version": args.version, "archive": str(archive),
                      "sha256": digest, "formula": str(formula), "source_url": source_url}, indent=2))


if __name__ == "__main__":
    main()
