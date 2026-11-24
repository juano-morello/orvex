#!/usr/bin/env python3
"""Create the demo credential files the engine, gateway and crashctl read.

    demo-credentials.py ensure DIR --unrevealed N
    demo-credentials.py restore DIR < BACKUP

DIR holds three files, each created at most once and never overwritten:

    keyring        the round secret keyring (ORVEX_SECRET_KEYRING_FILE)
    gateway-token  the gateway-to-engine service token (ORVEX_GATEWAY_TOKEN_FILE)
    players.json   bearer token to player ID, for alice and bob (ORVEX_PLAYERS_FILE)

The keyring key seals the secret config of every unsettled round. Losing it
strands those rounds, so the script never replaces a missing keyring while
the database still holds an unrevealed sealed round: pass the count from
the database as --unrevealed N, and a nonzero N exits 3 without writing.

The script prints only file names and the words created or kept. It never
prints a key or a token. Standard library only (Python 3.11 or newer).
"""

import argparse
import base64
import json
import os
import secrets
import subprocess
import sys
import tarfile
from pathlib import Path

PLAYERS = ("alice", "bob")
EXIT_KEY_MISSING = 3
EXIT_UNSAFE = 4


def keyring_text() -> str:
    key_id = "preview-" + secrets.token_hex(4)
    key = base64.b64encode(secrets.token_bytes(32)).decode("ascii")
    return f"active {key_id}\nkey {key_id} {key}\n"


def players_text() -> str:
    return json.dumps({secrets.token_hex(32): name for name in PLAYERS})


def gateway_token_text() -> str:
    return secrets.token_hex(32) + "\n"


FILES = {
    "keyring": keyring_text,
    "gateway-token": gateway_token_text,
    "players.json": players_text,
}


def create_once(path: Path, data: bytes) -> bool:
    """Write data to path with mode 0600 unless path exists. Return whether it did.

    The text goes to a private temporary file first and is linked into place,
    so a crash never leaves a truncated credential file that a later run
    would keep.
    """
    tmp = path.with_name(f".{path.name}.{secrets.token_hex(4)}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, "wb") as file:
            file.write(data)
        try:
            os.link(tmp, path)
        except FileExistsError:
            return False
        return True
    finally:
        os.unlink(tmp)


def ensure(directory: Path, unrevealed: int) -> int:
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    if directory.stat().st_mode & 0o077:
        print(f"{directory}: permissions are wider than 0700", file=sys.stderr)
        return EXIT_UNSAFE
    for name, make in FILES.items():
        path = directory / name
        if path.is_symlink() or (path.exists() and not path.is_file()):
            print(f"{name}: not a regular file", file=sys.stderr)
            return EXIT_UNSAFE
        if path.exists():
            if path.stat().st_mode & 0o077:
                print(f"{name}: permissions are wider than 0600", file=sys.stderr)
                return EXIT_UNSAFE
            print(f"kept {name}")
            continue
        if name == "keyring" and unrevealed:
            print(
                f"keyring: missing, and the database holds {unrevealed} unrevealed sealed "
                "round(s) that only the original key can open. Restore the original "
                "keyring (in a preview: ./workspace restore-keyring FILE), or discard "
                "the database (./workspace dispose). Nothing was written.",
                file=sys.stderr,
            )
            return EXIT_KEY_MISSING
        print(f"{'created' if create_once(path, make().encode('ascii')) else 'kept'} {name}")
    return 0


def adopt(directory: Path) -> int:
    """Copy the three known credential files from docker cp's legacy directory archive."""
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            parts = Path(member.name).parts
            if len(parts) != 2 or parts[0] != "orvex-demo" or parts[1] not in FILES:
                continue
            if not member.isfile() or not 0 < member.size <= 1 << 20:
                print(f"{parts[1]}: invalid legacy credential file", file=sys.stderr)
                return EXIT_UNSAFE
            with archive.extractfile(member) as source:
                data = source.read()
            created = create_once(directory / parts[1], data)
            print(f"{'adopted' if created else 'kept'} {parts[1]}")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = parser.add_subparsers(dest="command", required=True)
    one = sub.add_parser("ensure", help="create the missing credential files")
    one.add_argument("directory", type=Path)
    one.add_argument("--unrevealed", type=int, required=True, metavar="N")
    restore = sub.add_parser("restore", help="restore the original keyring from standard input")
    restore.add_argument("directory", type=Path)
    legacy = sub.add_parser("adopt", help="import earlier preview credentials from docker cp")
    legacy.add_argument("directory", type=Path)
    args = parser.parse_args(argv)
    if args.command == "adopt":
        return adopt(args.directory)
    if args.command == "restore":
        # sealing.MaxKeyringFileBytes is also the engine's input limit.
        data = sys.stdin.buffer.read((64 << 10) + 1)
        if not data or len(data) > 64 << 10:
            print("keyring: empty or oversized backup", file=sys.stderr)
            return EXIT_UNSAFE
        if (args.directory / "keyring").exists():
            print("keyring: a keyring already exists", file=sys.stderr)
            return EXIT_UNSAFE
        verifier = subprocess.run(
            ["go", "run", "-buildvcs=false", "./deployments/docker/check-keyring"],
            input=data, check=False,
        )
        if verifier.returncode:
            return EXIT_UNSAFE
        if not create_once(args.directory / "keyring", data):
            print("keyring: a keyring already exists", file=sys.stderr)
            return EXIT_UNSAFE
        print("restored keyring")
        return 0
    if args.unrevealed < 0:
        parser.error("--unrevealed must be nonnegative")
    return ensure(args.directory, args.unrevealed)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
