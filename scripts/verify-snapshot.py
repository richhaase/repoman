#!/usr/bin/env python3
"""Validate locally generated release artifacts; never install or publish them."""
import hashlib
import json
from pathlib import Path
import re
import tarfile

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / "dist"
artifacts = json.loads((DIST / "artifacts.json").read_text())
archives = [item for item in artifacts if item["type"] == "Archive"]
expected = {(os, arch) for os in ("darwin", "linux") for arch in ("amd64", "arm64")}
actual = {(item["goos"], item["goarch"]) for item in archives}
assert actual == expected and len(archives) == 4, actual
cask = (DIST / "homebrew/Casks/repoman.rb").read_text()
assert 'cask "repoman"' in cask and 'binary "repoman"' in cask
assert '"git"' in cask and '"gh"' in cask
assert not any(name in cask for name in ("TOKEN", "QUILL_", "Authorization"))
entries = re.findall(r'sha256 "([0-9a-f]{64})"\s+url "([^"]+)"', cask)
assert len(entries) == 4, entries
checksums = (DIST / "checksums.txt").read_text()
for item in archives:
    path = ROOT / item["path"]
    assert path.resolve().is_relative_to(DIST.resolve()), path
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    assert digest + "  " + item["name"] in checksums, item["name"]
    suffix = f'repoman_#{{version}}_{item["goos"]}_{item["goarch"]}.tar.gz'
    matches = [(sha, url) for sha, url in entries if url.endswith(suffix)]
    assert len(matches) == 1 and matches[0][0] == digest, suffix
    assert matches[0][1].startswith("https://github.com/richhaase/repoman/releases/download/"), matches
    with tarfile.open(path) as archive:
        names = set(archive.getnames())
        assert names == {"repoman", "README.md", "LICENSE"}, names
        binary = archive.getmember("repoman")
        assert binary.isfile() and binary.mode & 0o111, binary
print("Verified four archives, contents, executable modes, cask URLs/checksums, and git/gh dependencies")
