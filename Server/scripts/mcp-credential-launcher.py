#!/usr/bin/env python3
"""Hand a private Baby Tracker MCP credential to a desktop MCP client.

The credential stays in one protected file. This launcher never prints it,
never puts it in a command argument and never logs it. Standard library only,
Python 3.9 or later.

Modes (every path must be absolute):

  claude --credential-file FILE --bridge MCP_PROXY --url https://HOST/mcp [--ca-file PEM]
      Replaces itself (execve, same PID, same stdin/stdout) with the pinned
      mcp-proxy bridge in Streamable HTTP client mode. The bridge receives the
      credential only as API_ACCESS_TOKEN in a minimal environment.

  codex-headers --credential-file FILE
      Prints exactly {"Authorization": "Bearer <credential>"} for a Codex
      `http_headers_helper`. That captured stdout is the intended delivery
      channel: never run this mode interactively or log its output.

The credential file must be a regular, non-symlink file owned by the current
user with mode 0600 or 0400, in a directory owned by the current user with no
group or other access, holding exactly one btmcp1 credential (one trailing
newline is tolerated). Any failure prints one generic line to stderr, prints
nothing to stdout and exits 2, before any network access.
"""

import json
import os
import re
import stat
import sys

CREDENTIAL = re.compile(r"btmcp1\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.[A-Za-z0-9_-]{43}")
MAX_BYTES = 128


class Refused(Exception):
    """A deliberately detail-free refusal."""


def read_credential(path):
    if not os.path.isabs(path) or os.path.normpath(path) != path:
        raise Refused()
    uid = os.getuid()
    parent = os.lstat(os.path.dirname(path))
    if not stat.S_ISDIR(parent.st_mode) or parent.st_uid != uid or parent.st_mode & 0o077:
        raise Refused()
    before = os.lstat(path)
    if not stat.S_ISREG(before.st_mode):
        raise Refused()
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        opened = os.fstat(descriptor)
        if (not stat.S_ISREG(opened.st_mode) or opened.st_uid != uid or stat.S_IMODE(opened.st_mode) not in (0o600, 0o400)
                or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino) or opened.st_size > MAX_BYTES):
            raise Refused()
        data = os.read(descriptor, MAX_BYTES + 1)
    finally:
        os.close(descriptor)
    if len(data) > MAX_BYTES:
        raise Refused()
    try:
        text = data.decode("ascii")
    except UnicodeDecodeError:
        raise Refused() from None
    if text.endswith("\n"):
        text = text[:-1]
    if not CREDENTIAL.fullmatch(text):
        raise Refused()
    return text


def parse(arguments, allowed, required):
    values = {}
    if len(arguments) % 2:
        raise Refused()
    for name, value in zip(arguments[0::2], arguments[1::2]):
        if name not in allowed or name in values or not value or value.startswith("--"):
            raise Refused()
        values[name] = value
    if not required <= values.keys():
        raise Refused()
    return values


def valid_url(url):
    match = re.fullmatch(r"https://([A-Za-z0-9.-]+)(:[0-9]{1,5})?/mcp", url)
    return match is not None


def claude(arguments):
    values = parse(arguments, {"--credential-file", "--bridge", "--url", "--ca-file"}, {"--credential-file", "--bridge", "--url"})
    bridge, url, ca_file = values["--bridge"], values["--url"], values.get("--ca-file")
    if not os.path.isabs(bridge) or not os.path.isfile(bridge) or not os.access(bridge, os.X_OK) or not valid_url(url):
        raise Refused()
    if ca_file is not None and (not os.path.isabs(ca_file) or not os.path.isfile(ca_file)):
        raise Refused()
    credential = read_credential(values["--credential-file"])
    argv = [bridge, "--transport", "streamablehttp", "--log-level", "ERROR", "--no-debug"]
    if ca_file is not None:
        argv += ["--verify-ssl", ca_file]
    argv.append(url)
    environment = {"API_ACCESS_TOKEN": credential, "PATH": "/usr/bin:/bin"}
    for name in ("HOME", "LANG", "LC_ALL", "TMPDIR"):
        if name in os.environ:
            environment[name] = os.environ[name]
    os.execve(bridge, argv, environment)


def codex_headers(arguments):
    values = parse(arguments, {"--credential-file"}, {"--credential-file"})
    credential = read_credential(values["--credential-file"])
    sys.stdout.write(json.dumps({"Authorization": "Bearer " + credential}))
    sys.stdout.flush()


def main(argv):
    try:
        if len(argv) >= 2 and argv[1] == "claude":
            claude(argv[2:])
        elif len(argv) >= 2 and argv[1] == "codex-headers":
            codex_headers(argv[2:])
        else:
            raise Refused()
    except Exception:  # never echo paths, contents or exception text
        sys.stderr.write("baby-tracker-mcp-credential: refused (check the mode, flags and credential file)\n")
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
