#!/usr/bin/env python3
"""Independent MCP clients for Server/scripts/mcp-e2e.sh.

Runs inside the runner's disposable venv (hash-locked Python MCP SDK 1.27.1
and mcp-proxy 0.12.0). Modes:

  direct  the Python SDK's Streamable HTTP client straight to the server
  bridge  the Claude Desktop path: credential launcher -> pinned mcp-proxy
          (stdio <-> Streamable HTTP) driven by the Python SDK stdio client
  codex   Codex's native Streamable HTTP client (codex app-server, no model)
          with a throwaway CODEX_HOME and HOME and an http_headers_helper
  scan    synthetic-credential scan of every log the run produced

Results must equal the Go client's (expected.json, synthetic IDs/counts).
Output is sanitized: counts, statuses, exception class names and booleans.
Observed client behaviour on refusals is recorded, not prescribed; what is
asserted is that no refusal returns family data and nothing hangs.
"""

import asyncio
import base64
import json
import os
import re
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import time
from datetime import timedelta
from pathlib import Path
from urllib.parse import urlsplit

DIR = Path(os.environ["MCP_E2E_DIR"])
LOGS = Path(os.environ["MCP_E2E_LOGS"])
TOOLS = ["get_history", "get_summary", "list_babies"]
LEGACY = "2025-11-25"
PARTIAL = {}  # results so far, printed if a mode fails
GARBAGE = "btmcp1.00000000-0000-4000-8000-00000000e2ff." + "A" * 43


class Fail(Exception):
    pass


def check(condition, what):
    if not condition:
        raise Fail(what)


def emit(prefix, mode, values):
    print(f"{prefix} {mode} " + " ".join(f"{k}={v}" for k, v in values.items()), flush=True)


def note(mode, **values):
    emit("mcp-e2e python:", mode, values)


def credential(label):
    return (DIR / f"cred-{label}").read_text()


def expected():
    return json.loads((DIR / "expected.json").read_text())


def leaf_errors(error):
    """Flatten exception groups into sorted leaf class names (and HTTP status)."""
    found = []

    def walk(e):
        if isinstance(e, BaseExceptionGroup):
            for inner in e.exceptions:
                walk(inner)
            return
        name = type(e).__name__
        response = getattr(e, "response", None)
        if response is not None and getattr(response, "status_code", None):
            name += f"({response.status_code})"
        found.append(name)

    walk(error)
    return "+".join(sorted(set(found))) or "none"


def history_args(baby, limit=None):
    e = expected()
    args = {"baby_id": baby, "start": e["range_start"], "end": e["range_end"]}
    if limit:
        args["limit"] = limit
    return args


def summary_args(baby):
    e = expected()
    return {"baby_id": baby, "start": e["range_start"], "end": e["range_end"], "timezone": "Europe/Zurich"}


async def exercise(session, mode):
    """Catalog plus each tool, compared with the Go client's results."""
    e = expected()
    tools = (await session.list_tools()).tools
    names = sorted(t.name for t in tools)
    check(names == TOOLS, f"{mode} catalog")
    check(all(t.annotations and t.annotations.readOnlyHint and t.annotations.destructiveHint is False for t in tools), f"{mode} annotations")
    babies = await session.call_tool("list_babies", {})
    check(not babies.isError, f"{mode} list_babies error")
    baby_ids = sorted(b["meta"]["id"] for b in babies.structuredContent["babies"])
    history = await session.call_tool("get_history", history_args(e["baby_a"]))
    check(not history.isError, f"{mode} get_history error")
    history_ids = sorted(entry["id"] for entry in history.structuredContent["entries"])
    timers = len(history.structuredContent["active_timers"])
    text_matches = json.loads(history.content[0].text) == history.structuredContent
    summary = await session.call_tool("get_summary", summary_args(e["baby_a"]))
    check(not summary.isError, f"{mode} get_summary error")
    totals_equal = summary.structuredContent["totals"] == e["totals"]
    wrong = await session.call_tool("get_history", history_args(e["baby_b"]))
    wrong_code = json.loads(wrong.content[0].text)["error"]["code"] if wrong.isError else "data_returned"
    check(baby_ids == e["babies_a"] and history_ids == e["history_ids"] and timers == e["active_timers"] and totals_equal, f"{mode} results differ from the Go client")
    check(text_matches and wrong_code == "baby_not_found", f"{mode} envelope or cross-family")
    return {"tools": len(names), "read_only": "all", "babies": len(baby_ids), "history": len(history_ids), "timers": timers,
            "summary_equal_go": totals_equal, "text_equals_structured": text_matches, "cross_family": wrong_code, "equal_to_go": True}


# --- raw helpers (stall a request in its body read; plain HTTP probes) -------

def canonical_host():
    return urlsplit(os.environ["MCP_E2E_URL"]).netloc


def stall(token):
    """Hold a server slot: headers and half a body straight to the app port."""
    app = urlsplit(os.environ["MCP_E2E_APP"])
    conn = socket.create_connection((app.hostname, app.port), timeout=10)
    conn.sendall((f"POST /mcp HTTP/1.1\r\nHost: {canonical_host()}\r\nContent-Type: application/json\r\n"
                  f"Accept: application/json, text/event-stream\r\nMCP-Protocol-Version: {LEGACY}\r\n"
                  f"Authorization: Bearer {token}\r\nContent-Length: 512\r\n\r\n{{\"jsonrpc\":\"2.0\",").encode())
    return conn


# --- direct ---------------------------------------------------------------

async def direct():
    import anyio
    import httpx
    from mcp import ClientSession
    from mcp.client.streamable_http import streamable_http_client

    url, ca = os.environ["MCP_E2E_URL"], os.environ["MCP_E2E_CA"]

    def client(token):
        return httpx.AsyncClient(headers={"Authorization": f"Bearer {token}"}, verify=ssl.create_default_context(cafile=ca),
                                 timeout=httpx.Timeout(15), follow_redirects=False)

    token = credential("py-a")
    results = PARTIAL
    async with client(token) as http, streamable_http_client(url, http_client=http) as (read, write, session_id):
        async with ClientSession(read, write, read_timeout_seconds=timedelta(seconds=15)) as session:
            init = await session.initialize()
            results["negotiated"] = init.protocolVersion
            results["session_id"] = "absent" if session_id() is None else "present"
            results.update(await exercise(session, "direct"))
    check(results["negotiated"] == LEGACY and results["session_id"] == "absent", "direct negotiation")

    async def attempt(label, token, before=None, after=None):
        """Run initialize + call_tool, bounded; report the observable outcome."""
        outcome, data = "ok", False
        try:
            with anyio.fail_after(12):
                async with client(token) as http, streamable_http_client(url, http_client=http) as (read, write, _):
                    async with ClientSession(read, write, read_timeout_seconds=timedelta(seconds=8)) as session:
                        await session.initialize()
                        if before:
                            await before()
                        try:
                            result = await session.call_tool("list_babies", {})
                            data = bool(result.structuredContent)
                            outcome = "tool_error" if result.isError else "ok"
                        finally:
                            if after:
                                after()
        except TimeoutError:
            outcome = "hung>12s"
        except BaseException as error:  # noqa: BLE001 - the class is the observation
            outcome = leaf_errors(error)
        return outcome, data

    # Two requests at once in one SDK session on one credential.
    async def concurrent_direct():
        async with client(token) as http, streamable_http_client(url, http_client=http) as (read, write, _):
            async with ClientSession(read, write, read_timeout_seconds=timedelta(seconds=8)) as session:
                await session.initialize()
                outcomes = await asyncio.gather(session.list_tools(), session.call_tool("list_babies", {}), return_exceptions=True)
                return "+".join("ok" if not isinstance(o, BaseException) else leaf_errors(o) for o in outcomes)
    try:
        with anyio.fail_after(20):
            results["concurrent_list_call"] = await concurrent_direct()
    except TimeoutError:
        results["concurrent_list_call"] = "hung>20s"
    except BaseException as error:  # noqa: BLE001
        results["concurrent_list_call"] = "session_ended:" + leaf_errors(error)
    check("hung" not in results["concurrent_list_call"], "direct concurrent requests hung")
    await asyncio.sleep(1.2)

    # Pre-parse 401 (id:null) on initialize: a never-issued credential.
    unauthorized, leaked = await attempt("unauthorized", GARBAGE)
    check(unauthorized != "ok" and not leaked and "hung" not in unauthorized, "direct garbage credential")
    results["unauthorized"] = unauthorized

    # 429 concurrency_limited (id:null) inside a live session: the same
    # credential's earlier request is still in flight.
    held = []

    async def hold_same():
        held.append(stall(token))
        await asyncio.sleep(0.5)

    concurrent, leaked = await attempt("concurrency", token, hold_same, lambda: [c.close() for c in held])
    check(not leaked and "hung" not in concurrent, "direct concurrency refusal")
    results["concurrency_429"] = concurrent

    # 503 server_busy (id:null): both global MCP slots held by other
    # credentials' stalled bodies.
    holders = []

    async def hold_all():
        holders.extend([stall(credential("codex-a")), stall(credential("b"))])
        await asyncio.sleep(0.5)

    await asyncio.sleep(1.2)  # the concurrency refusal cache lasts at most 1 s
    busy, leaked = await attempt("busy", token, hold_all, lambda: [c.close() for c in holders])
    check(not leaked and "hung" not in busy, "direct busy refusal")
    results["busy_503"] = busy
    await asyncio.sleep(0.5)
    recovered, _ = await attempt("recovered", token)
    check(recovered == "ok", "direct fresh session after refusals")
    results["fresh_session_after"] = recovered

    # Transport details through httpx, outside the SDK.
    async with client(token) as http:
        get = await http.get(url, headers={"Accept": "text/event-stream", "MCP-Protocol-Version": LEGACY})
        anonymous = await httpx.AsyncClient(verify=ssl.create_default_context(cafile=ca)).post(
            url, json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, headers={"Accept": "application/json, text/event-stream", "MCP-Protocol-Version": LEGACY})
    check(get.status_code == 405 and anonymous.status_code == 401 and anonymous.json()["id"] is None, "direct raw probes")
    check(anonymous.headers.get("www-authenticate", "").startswith("Bearer") and get.headers.get("cache-control") == "no-store", "direct raw headers")
    results["get"] = get.status_code
    results["anonymous"] = f"{anonymous.status_code}/id_null"
    emit("mcp-e2e proof:", "python-direct", results)


# --- bridge (the Claude Desktop path) --------------------------------------

def launcher_args(label, url=None, ca=True):
    args = [os.environ["MCP_E2E_LAUNCHER"], "claude", "--credential-file", str(DIR / f"cred-{label}"),
            "--bridge", os.environ["MCP_E2E_BRIDGE"], "--url", url or os.environ["MCP_E2E_URL"]]
    if ca:
        args += ["--ca-file", os.environ["MCP_E2E_CA"]]
    return args


def bridge_env():
    # Like a GUI app: no inherited credential, a plain PATH and HOME.
    return {"PATH": "/usr/bin:/bin", "HOME": os.environ.get("HOME", "/tmp")}


async def bridge_session(args, errlog, body):
    from mcp import ClientSession, StdioServerParameters
    from mcp.client.stdio import stdio_client

    params = StdioServerParameters(command=os.environ["MCP_E2E_PYTHON"], args=args, env=bridge_env())
    async with stdio_client(params, errlog=errlog) as (read, write):
        async with ClientSession(read, write, read_timeout_seconds=timedelta(seconds=15)) as session:
            init = await session.initialize()
            return await body(session, init)


async def bridge_failure(label, args, errlog):
    """A bridge whose upstream cannot initialize: what the desktop sees."""
    import anyio

    started = time.monotonic()
    outcome, catalog = "ok", False
    try:
        with anyio.fail_after(30):
            async def body(session, init):
                nonlocal catalog
                tools = await session.list_tools()
                catalog = len(tools.tools) > 0
                return None
            await bridge_session(args, errlog, body)
    except TimeoutError:
        outcome = "hung>30s"
    except BaseException as error:  # noqa: BLE001
        outcome = leaf_errors(error)
    return outcome, catalog, round(time.monotonic() - started, 1)


def bridge_process(args, errlog):
    """The bridge as a raw child process speaking newline-delimited JSON-RPC."""
    process = subprocess.Popen([os.environ["MCP_E2E_PYTHON"], *args], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errlog, env=bridge_env())
    request = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": LEGACY, "capabilities": {}, "clientInfo": {"name": "mcp-e2e", "version": "1"}}}
    process.stdin.write((json.dumps(request) + "\n").encode())
    process.stdin.flush()
    line = process.stdout.readline()
    check(b'"result"' in line, "raw bridge initialize")
    process.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
    process.stdin.flush()
    return process


def descendants(pid):
    out = subprocess.run(["pgrep", "-P", str(pid)], capture_output=True, text=True).stdout.split()
    found = [int(p) for p in out]
    for child in list(found):
        found += descendants(child)
    return found


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


async def bridge():
    import anyio

    results = PARTIAL
    errlog = open(LOGS / "bridge-stderr.log", "a")

    # Startup, catalog, every tool, compared with Go.
    async def happy(session, init):
        results["upstream_init"] = "ok"
        results.update(await exercise(session, "bridge"))
        # A call the desktop abandons: cancelled locally, session continues.
        with anyio.move_on_after(0.001) as scope:
            await session.call_tool("get_summary", summary_args(expected()["baby_a"]))
        results["abandoned_call"] = "cancelled" if scope.cancelled_caught else "completed"
        await asyncio.sleep(0.5)
        after = await session.call_tool("list_babies", {})
        results["after_abandon"] = "ok" if not after.isError else "tool_error"
        return init

    try:
        with anyio.fail_after(60):
            await bridge_session(launcher_args("py-a"), errlog, happy)
    except BaseException as error:  # noqa: BLE001
        raise Fail("bridge happy path: " + leaf_errors(error)) from None
    check(results["after_abandon"] == "ok", "bridge session after an abandoned call")

    # Two requests at once through one bridge on one credential: the server
    # admits one and refuses the other (429 concurrency_limited, id:null).
    async def concurrent(session, init):
        outcomes = await asyncio.gather(session.list_tools(), session.call_tool("list_babies", {}), return_exceptions=True)
        return "+".join("ok" if not isinstance(o, BaseException) else leaf_errors(o) for o in outcomes)

    try:
        with anyio.fail_after(60):
            results["concurrent_list_call"] = await bridge_session(launcher_args("py-a"), errlog, concurrent)
    except TimeoutError:
        results["concurrent_list_call"] = "hung>60s"
    except BaseException as error:  # noqa: BLE001
        results["concurrent_list_call"] = "session_ended:" + leaf_errors(error)
    check("hung" not in results["concurrent_list_call"], "bridge concurrent requests hung")
    await asyncio.sleep(1.2)

    # A refusal while the bridge is mid-session: hold this credential's
    # in-flight slot, then call.
    async def refused(session, init):
        held = stall(credential("py-a"))
        await asyncio.sleep(0.5)
        try:
            outcome = await session.call_tool("list_babies", {})
            first = "tool_error" if outcome.isError else "ok"
        except BaseException as error:  # noqa: BLE001
            first = leaf_errors(error)
        finally:
            held.close()
        await asyncio.sleep(1.2)
        try:
            again = await session.call_tool("list_babies", {})
            second = "tool_error" if again.isError else "ok"
        except BaseException as error:  # noqa: BLE001
            second = leaf_errors(error)
        return first, second

    try:
        with anyio.fail_after(60):
            first, second = await bridge_session(launcher_args("py-a"), errlog, refused)
    except TimeoutError:
        first, second = "hung>60s", "n/a"
    except BaseException as error:  # noqa: BLE001
        first, second = "session_ended", leaf_errors(error)
    check("hung" not in first, "bridge refusal hung")
    results["mid_session_429"] = first
    results["next_call_after_429"] = second

    # Startup failures must not advertise a usable catalog.
    closed = socket.socket()
    closed.bind(("127.0.0.1", 0))
    closed_port = closed.getsockname()[1]
    closed.close()
    revoked_or_garbage = DIR / "cred-garbage"
    revoked_or_garbage.write_text(GARBAGE)
    os.chmod(revoked_or_garbage, 0o600)
    for name, args in [("auth_401", launcher_args("garbage")),
                       ("network", launcher_args("py-a", url=f"https://localhost:{closed_port}/mcp")),
                       ("tls_untrusted", launcher_args("py-a", ca=False))]:
        outcome, catalog, seconds = await bridge_failure(name, args, errlog)
        check(outcome != "ok" and not catalog and "hung" not in outcome, f"bridge {name} startup failure")
        results[name] = f"{outcome}/catalog={str(catalog).lower()}/{seconds}s"

    # Unsafe credential files are refused by the launcher before any request.
    unsafe = {}
    loose = DIR / "unsafe"
    loose.mkdir(mode=0o700, exist_ok=True)
    good = credential("py-a")
    (loose / "mode0644").write_text(good); os.chmod(loose / "mode0644", 0o644)
    (loose / "malformed").write_text(good + "x"); os.chmod(loose / "malformed", 0o600)
    (loose / "link").symlink_to(DIR / "cred-py-a")
    open_dir = DIR / "open"
    open_dir.mkdir(mode=0o755, exist_ok=True); os.chmod(open_dir, 0o755)
    (open_dir / "cred").write_text(good); os.chmod(open_dir / "cred", 0o600)
    for name, path in [("missing", loose / "absent"), ("mode0644", loose / "mode0644"), ("malformed", loose / "malformed"),
                       ("symlink", loose / "link"), ("open_directory", open_dir / "cred"), ("relative", Path("cred-py-a"))]:
        run = subprocess.run([os.environ["MCP_E2E_PYTHON"], os.environ["MCP_E2E_LAUNCHER"], "claude", "--credential-file", str(path),
                              "--bridge", os.environ["MCP_E2E_BRIDGE"], "--url", os.environ["MCP_E2E_URL"]],
                             capture_output=True, timeout=10, env=bridge_env(), cwd=str(DIR))
        errlog.write(run.stderr.decode(errors="replace"))
        check(run.returncode == 2 and run.stdout == b"" and str(path) not in run.stderr.decode(errors="replace"), f"launcher {name}")
        unsafe[name] = run.returncode
    results["unsafe_files_refused"] = len(unsafe)

    # Process lifecycle: EOF on stdin, SIGINT and SIGTERM each end the bridge
    # (the launcher exec'd into it: same PID) and leave no child behind.
    for name, signum in [("eof", None), ("sigint", signal.SIGINT), ("sigterm", signal.SIGTERM)]:
        process = bridge_process(launcher_args("py-a"), errlog)
        children = descendants(process.pid)
        started = time.monotonic()
        if signum is None:
            process.stdin.close()
        else:
            process.send_signal(signum)
        try:
            code = process.wait(timeout=10)
            observed = f"{code}/{round(time.monotonic() - started, 1)}s"
        except subprocess.TimeoutExpired:
            # Recorded, not required: desktop hosts close stdin, then SIGTERM.
            check(name == "sigint", f"bridge did not exit on {name}")
            process.terminate()
            try:
                code = process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                raise Fail("bridge ignored SIGINT and SIGTERM") from None
            observed = f"ignored_10s->sigterm_exit_{code}"
        leftovers = [c for c in children if alive(c)]
        check(not leftovers, f"bridge {name} left a child")
        results[f"exit_{name}"] = observed
    errlog.close()
    emit("mcp-e2e proof:", "python-bridge", results)


# --- codex ------------------------------------------------------------------

class AppServer:
    """Minimal newline-delimited JSON-RPC driver for `codex app-server`."""

    def __init__(self, home, log):
        environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(home), "CODEX_HOME": str(home / "codex"),
                       "CODEX_CA_CERTIFICATE": os.environ["MCP_E2E_CA"], "TMPDIR": str(home / "tmp")}
        self.process = subprocess.Popen([os.environ["CODEX_BIN"], "app-server", "--strict-config"], stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=log, env=environment, cwd=str(home))
        self.next_id = 0
        self.notifications = []

    def request(self, method, params, timeout=60):
        self.next_id += 1
        ident = self.next_id
        self.process.stdin.write((json.dumps({"id": ident, "method": method, "params": params}) + "\n").encode())
        self.process.stdin.flush()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            line = self.process.stdout.readline()
            if not line:
                raise Fail(f"codex app-server closed during {method}")
            message = json.loads(line)
            if message.get("id") == ident and ("result" in message or "error" in message):
                return message
            if "method" in message and "id" in message:
                # A server request (approval, elicitation): decline.
                self.process.stdin.write((json.dumps({"id": message["id"], "error": {"code": -32601, "message": "declined"}}) + "\n").encode())
                self.process.stdin.flush()
            elif "method" in message:
                self.notifications.append(message["method"])
        raise Fail(f"codex {method} timed out")

    def notify(self, method):
        self.process.stdin.write((json.dumps({"method": method}) + "\n").encode())
        self.process.stdin.flush()

    def close(self):
        self.process.stdin.close()
        try:
            return self.process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            self.process.kill()
            return "killed"


def codex_home(label, name):
    home = DIR / f"codex-home-{name}"
    (home / "codex").mkdir(parents=True, mode=0o700)
    (home / "tmp").mkdir(mode=0o700)
    helper = f'{os.environ["MCP_E2E_PYTHON"]} {os.environ["MCP_E2E_LAUNCHER"]} codex-headers --credential-file {DIR / ("cred-" + label)}'
    config = f"""check_for_update_on_startup = false

[mcp_servers.baby_tracker_readonly]
url = "{os.environ["MCP_E2E_URL"]}"
http_headers_helper = "{helper}"
enabled_tools = ["list_babies", "get_history", "get_summary"]
startup_timeout_sec = 15
tool_timeout_sec = 15
"""
    (home / "codex" / "config.toml").write_text(config)
    return home


def codex_cli(home, *args):
    environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(home), "CODEX_HOME": str(home / "codex")}
    return subprocess.run([os.environ["CODEX_BIN"], *args], capture_output=True, text=True, timeout=60, env=environment, cwd=str(home))


def codex_status(server, name):
    status = server.request("mcpServerStatus/list", {"serverName": "baby_tracker_readonly"}, timeout=90)
    check("result" in status, f"codex {name} status list failed")
    entries = [s for s in status["result"]["data"] if s.get("name") == "baby_tracker_readonly"]
    check(len(entries) == 1, f"codex {name} server missing")
    return entries[0]


def codex():
    results = PARTIAL
    version = codex_cli(DIR, "--version")
    results["codex"] = version.stdout.strip().replace(" ", "-")
    log = open(LOGS / "codex-stderr.log", "a")

    home = codex_home("codex-a", "ok")
    listed = codex_cli(home, "mcp", "list", "--json")
    check(listed.returncode == 0, "codex mcp list")
    configured = json.loads(listed.stdout)
    entry = [s for s in configured if s.get("name") == "baby_tracker_readonly"]
    check(len(entry) == 1, "codex mcp list lacks the server")
    results["config_parsed"] = True

    server = AppServer(home, log)
    init = server.request("initialize", {"clientInfo": {"name": "baby-tracker-mcp-e2e", "title": None, "version": "1"}})
    check("result" in init, "codex initialize")
    server.notify("initialized")
    status = codex_status(server, "ok")
    tools = sorted(status.get("tools", {}).keys()) if isinstance(status.get("tools"), dict) else sorted(t.get("name") for t in status.get("tools", []))
    results["status_tools"] = len(tools)
    results["auth_status"] = status.get("authStatus")
    check(tools == TOOLS, "codex tool discovery")
    thread = server.request("thread/start", {"ephemeral": True, "cwd": str(home)})
    if "result" in thread:
        thread_id = thread["result"]["thread"]["id"]
        e = expected()
        babies = server.request("mcpServer/tool/call", {"server": "baby_tracker_readonly", "threadId": thread_id, "tool": "list_babies", "arguments": {}})
        history = server.request("mcpServer/tool/call", {"server": "baby_tracker_readonly", "threadId": thread_id, "tool": "get_history", "arguments": history_args(e["baby_a"])})
        summary = server.request("mcpServer/tool/call", {"server": "baby_tracker_readonly", "threadId": thread_id, "tool": "get_summary", "arguments": summary_args(e["baby_a"])})
        check(all("result" in r and not r["result"].get("isError") for r in (babies, history, summary)), "codex tool calls")
        baby_ids = sorted(b["meta"]["id"] for b in babies["result"]["structuredContent"]["babies"])
        history_ids = sorted(x["id"] for x in history["result"]["structuredContent"]["entries"])
        check(baby_ids == e["babies_a"] and history_ids == e["history_ids"] and summary["result"]["structuredContent"]["totals"] == e["totals"], "codex results differ from Go")
        results["tool_calls"] = "3/equal_to_go"

        def outcome():
            started = time.monotonic()
            reply = server.request("mcpServer/tool/call", {"server": "baby_tracker_readonly", "threadId": thread_id, "tool": "list_babies", "arguments": {}})
            elapsed = round(time.monotonic() - started, 1)
            if "error" in reply:
                kind = "timeout" if "timed out" in reply["error"].get("message", "") else "rpc_error"
            else:
                kind = "tool_error" if reply["result"].get("isError") else "ok"
            return f"{kind}/{elapsed}s"

        # A pre-body refusal (429 concurrency_limited, id:null) mid-session:
        # this credential's earlier request is still in flight.
        held = stall(credential("codex-a"))
        time.sleep(0.5)
        results["mid_session_429"] = outcome()
        held.close()
        time.sleep(1.5)
        results["next_call_after_429"] = outcome()
        check(results["next_call_after_429"].startswith("ok/"), "codex did not recover after a refusal")
    else:
        results["tool_calls"] = "thread_start_refused:" + str(thread["error"].get("code"))
    results["exit_ok_server"] = server.close()

    # A credential the server rejects (never issued): what Codex reports.
    bad = DIR / "cred-codex-garbage"
    bad.write_text(GARBAGE)
    os.chmod(bad, 0o600)
    home = codex_home("codex-garbage", "unauthorized")
    server = AppServer(home, log)
    server.request("initialize", {"clientInfo": {"name": "baby-tracker-mcp-e2e", "title": None, "version": "1"}})
    server.notify("initialized")
    failed = codex_status(server, "unauthorized")
    failed_tools = failed.get("tools") or {}
    check(len(failed_tools) == 0, "codex advertised tools for a rejected credential")
    results["unauthorized_status"] = failed.get("authStatus")
    reason = failed.get("toolsError") or ""
    results["unauthorized_error_mentions_401"] = "401" in reason or "Unauthorized" in reason
    results["unauthorized_tools"] = len(failed_tools)
    results["exit_unauthorized_server"] = server.close()
    log.close()
    emit("mcp-e2e proof:", "python-codex", results)


# --- scan -------------------------------------------------------------------

def scan():
    """Report only presence/absence of any issued credential in any log."""
    secrets = []
    for path in sorted(DIR.glob("cred-*")):
        value = path.read_text().strip()
        if not value.startswith("btmcp1."):
            continue
        secret = value.rsplit(".", 1)[1]
        raw = base64.urlsafe_b64decode(secret + "=")
        secrets += [value, secret, base64.b64encode(raw).decode().rstrip("="), raw.hex()]
    # Every log, plus everything Codex wrote under its throwaway homes (its
    # own logs, session and state files).
    files = [p for p in LOGS.rglob("*") if p.is_file() and not p.is_symlink()]
    files += [p for home in DIR.glob("codex-home-*") for p in home.rglob("*") if p.is_file() and not p.is_symlink()]
    hits = 0
    for path in files:
        data = path.read_bytes()
        hits += sum(1 for s in secrets if s.encode() in data)
    check(secrets, "no credentials to scan for")
    codex_files = sum(1 for p in files if "codex-home-" in str(p))
    emit("mcp-e2e proof:", "python-scan", {"credentials": len(secrets) // 4, "patterns": len(secrets), "files": len(files), "codex_home_files": codex_files, "matches": hits})
    check(hits == 0, "a credential appeared in a log")


def main():
    mode = sys.argv[1] if len(sys.argv) == 2 else ""
    try:
        if mode == "direct":
            asyncio.run(direct())
        elif mode == "bridge":
            asyncio.run(bridge())
        elif mode == "codex":
            codex()
        elif mode == "scan":
            scan()
        else:
            raise Fail("mode must be direct, bridge, codex or scan")
    except Fail as failure:
        if PARTIAL:
            note(mode + "-partial", **PARTIAL)
        print(f"mcp-e2e python: {mode} FAILED {failure}", flush=True)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
