"""The Windows build copies a committed tree out of the checkout: it must carry everything the renderer reads."""
import re
import subprocess
import tarfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "desktop" / "scripts" / "build-windows.sh"


def test_the_windows_build_archive_has_the_theme_files(tmp_path):
    """desktop/src/theme.ts globs ../../shared/themes: an archive of desktop/ alone made an installer whose window stayed blank."""
    m = re.search(r'archive --format=tar -o "\$root/src\.tar" HEAD ([\w/ ]+)$', SCRIPT.read_text(), re.M)
    assert m, "build-windows.sh no longer archives HEAD with git archive"
    tar = tmp_path / "src.tar"
    subprocess.run(["git", "-C", str(ROOT), "archive", "--format=tar", "-o", str(tar), "HEAD", *m.group(1).split()], check=True)
    with tarfile.open(tar) as t:
        names = t.getnames()
    assert any(n.startswith("shared/themes/") and n.endswith(".json") for n in names), m.group(1)
    assert "desktop/package.json" in names
