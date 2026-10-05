"""A scriptable plugin that speaks the wire protocol by hand (no SDK).

The supervisor tests drive it with inputs.mode to provoke every behavior a
plugin can have, including hostile and broken ones.
"""

import base64
import json
import os
import resource
import select
import signal
import socket
import struct
import subprocess
import sys
import time

sock = socket.socket(fileno=int(os.environ["OPENGTM_PLUGIN_FD"]))


def send_raw(body: bytes) -> None:
    sock.sendall(struct.pack(">I", len(body)) + body)


def send(msg: dict) -> None:
    send_raw(json.dumps(msg).encode())


def recv() -> dict:
    hdr = b""
    while len(hdr) < 4:
        chunk = sock.recv(4 - len(hdr))
        if not chunk:
            raise EOFError
        hdr += chunk
    (n,) = struct.unpack(">I", hdr)
    body = b""
    while len(body) < n:
        chunk = sock.recv(n - len(body))
        if not chunk:
            raise EOFError
        body += chunk
    return json.loads(body)


protocols = [1]
if os.environ.get("RAW_HELLO_PROTOCOL"):
    protocols = [int(os.environ["RAW_HELLO_PROTOCOL"])]
send({"type": "hello", "protocols": protocols, "sdk": {"name": "raw", "version": "0", "language": "python"}})
init = recv()
inputs = init["inputs"]
mode = inputs.get("mode", "ok")


def result(records=None, **extra):
    send({"type": "result", "records": records if records is not None else [{"fields": {"ok": True}}], **extra})


def spawn_tree(pidfile):
    """child in the group, a setsid child, and an orphaned double fork."""
    pids = {}
    pids["same_group"] = subprocess.Popen(["sleep", "300"]).pid
    pids["setsid"] = subprocess.Popen(["sleep", "300"], start_new_session=True).pid
    # double fork: the intermediate exits, the grandchild is re-parented to init
    out = subprocess.check_output(
        ["sh", "-c", "(setsid sleep 300 >/dev/null 2>&1 & echo $!)"], text=True)
    pids["orphan"] = int(out.strip())
    pids["self"] = os.getpid()
    with open(pidfile, "w") as f:
        json.dump(pids, f)


if mode == "ok":
    result()
elif mode == "echo":
    result([{"fields": {"echo": inputs.get("value")}, "evidence": {"note": "from plugin"}}])
elif mode == "sleep":
    time.sleep(float(inputs.get("seconds", 300)))
    result()
elif mode == "ignore_cancel":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    spawn_tree(inputs["pidfile"])
    while True:
        time.sleep(1)
elif mode == "tree":
    spawn_tree(inputs["pidfile"])
    time.sleep(300)
elif mode == "tree_then_ok":
    spawn_tree(inputs["pidfile"])
    result()
elif mode == "cooperative":
    send({"type": "log", "message": "ready"})
    while True:
        r, _, _ = select.select([sock], [], [], 0.05)
        if r:
            msg = recv()
            if msg["type"] == "cancel":
                send({"type": "failure", "code": "cancelled", "message": "stopped on request: " + msg["reason"]})
                break
elif mode == "segv":
    os.kill(os.getpid(), signal.SIGSEGV)
elif mode == "sigkill_self":
    os.kill(os.getpid(), signal.SIGKILL)
elif mode == "exit1":
    print("fatal: something broke " + inputs.get("leak", ""), file=sys.stderr)
    sys.exit(1)
elif mode == "exit0":
    sys.exit(0)
elif mode == "hang_after_result":
    result()
    time.sleep(300)
elif mode == "garbage":
    send_raw(b"this is not json")
elif mode == "array_frame":
    send_raw(b"[1,2,3]")
elif mode == "unknown_type":
    send({"type": "teleport"})
elif mode == "second_hello":
    send({"type": "hello", "protocols": [1], "sdk": {"name": "x", "version": "0", "language": "py"}})
    time.sleep(5)
elif mode == "oversize":
    sock.sendall(struct.pack(">I", 0x7FFFFFFF))
    time.sleep(5)
elif mode == "truncated":
    sock.sendall(struct.pack(">I", 100) + b"{")
    sys.exit(0)
elif mode == "fail":
    f = {"type": "failure", "code": inputs["code"], "message": inputs.get("message", "")}
    if "retryable" in inputs:
        f["retryable"] = inputs["retryable"]
    send(f)
elif mode == "env":
    ppid = os.getppid()
    leaks = {}
    for path in (f"/proc/{ppid}/environ", f"/proc/{ppid}/mem", f"/proc/{ppid}/cmdline"):
        try:
            with open(path, "rb") as fh:
                fh.read(16)
            leaks[path] = "readable"
        except OSError as e:
            leaks[path] = type(e).__name__
    try:
        fds = sorted(int(x) for x in os.listdir("/proc/self/fd"))
    except OSError:
        fds = []
    home = os.environ.get("HOME", "")
    result([{"fields": {
        "env": dict(os.environ),
        "secret_names": sorted(init["secrets"]),
        # reversed so the host's output redaction does not hide what was delivered
        "secret_rev": {k: v[::-1] for k, v in init["secrets"].items()},
        "cwd": os.getcwd(),
        "home": home,
        "home_listing": sorted(os.listdir(home)) if home else [],
        "tmpdir_mode": oct(os.stat(home).st_mode & 0o777) if home else "",
        "fds": fds,
        "proc_leaks": leaks,
        "uid": os.getuid(),
    }}])
elif mode == "scan_fs":
    # look for a needle anywhere a plugin could reasonably look
    needle = inputs["needle"].encode()
    found = []
    roots = [os.environ.get("HOME", "/nonexistent"), os.environ.get("TMPDIR", "/nonexistent"), os.getcwd()]
    for root in roots:
        for dirpath, _, files in os.walk(root):
            for name in files:
                p = os.path.join(dirpath, name)
                try:
                    with open(p, "rb") as fh:
                        if needle in fh.read(1 << 20):
                            found.append(p)
                except OSError:
                    pass
    for pid in os.listdir("/proc"):
        if pid.isdigit() and int(pid) != os.getpid():
            for name in ("environ", "cmdline"):
                try:
                    with open(f"/proc/{pid}/{name}", "rb") as fh:
                        if needle in fh.read():
                            found.append(f"/proc/{pid}/{name}")
                except OSError:
                    pass
    result([{"fields": {"found": found}}])
elif mode == "write_files":
    with open(os.path.join(os.environ["HOME"], "scratch.txt"), "w") as fh:
        fh.write(inputs.get("content", "x"))
    result()
elif mode == "fetch":
    out = []
    for i, spec in enumerate(inputs["requests"]):
        send({"type": "fetch", "id": i + 1, **spec})
        reply = recv()
        row = {"id": reply["id"], "status": reply.get("status"), "error": reply.get("error"),
               "body": reply.get("body"), "evidence": reply.get("evidence")}
        out.append(row)
    result([{"fields": {"replies": out}}])
elif mode == "fetch_concurrent":
    n = inputs["n"]
    for i in range(n):
        send({"type": "fetch", "id": i + 1, "url": inputs["url"]})
    got = [recv() for _ in range(n)]
    result([{"fields": {"ids": sorted(g["id"] for g in got), "statuses": [g.get("status") for g in got]}}])
elif mode == "leak":
    secret = next(iter(init["secrets"].values()))
    send({"type": "log", "message": "token is " + secret})
    send({"type": "progress", "message": "progress " + secret})
    print("stdout " + secret)
    result([{"fields": {"token": secret, "nested": {"list": [secret]}}, "evidence": {"auth": "Bearer " + secret}}],
           provider_error="")
elif mode == "limits":
    result([{"fields": {
        "nofile": resource.getrlimit(resource.RLIMIT_NOFILE),
        "address_space": resource.getrlimit(resource.RLIMIT_AS),
        "cpu": resource.getrlimit(resource.RLIMIT_CPU),
        "core": resource.getrlimit(resource.RLIMIT_CORE),
    }}])
elif mode == "oom":
    try:
        blob = bytearray(2 * 1024 * 1024 * 1024)
        result([{"fields": {"allocated": len(blob)}}])
    except MemoryError:
        send({"type": "failure", "code": "plugin_exception", "message": "MemoryError under the address-space limit"})
elif mode == "fd_bomb":
    files = []
    try:
        while True:
            files.append(open("/dev/null"))
    except OSError as e:
        result([{"fields": {"opened": len(files), "errno": e.errno}}])
elif mode == "many_records":
    n = inputs["n"]
    batch = []
    for i in range(n):
        batch.append({"fields": {"i": i, "pad": "x" * inputs.get("pad", 0)}})
        if len(batch) == 100:
            send({"type": "records", "records": batch})
            batch = []
    result(batch)
elif mode == "records_and_result":
    send({"type": "records", "records": [{"fields": {"n": 1}}, {"fields": {"n": 2}}]})
    result([{"fields": {"n": 3}}], pages=7, cost_usd=0.5, stopped="max_pages")
elif mode == "bad_output":
    result([{"fields": inputs["fields"]}])
elif mode == "no_fields":
    send({"type": "result", "records": [{"evidence": {}}]})
elif mode == "progress":
    for i in range(50):
        send({"type": "progress", "pages": i, "records": i})
    result()
elif mode == "logspam":
    for i in range(1000):
        send({"type": "log", "message": f"line {i}"})
    result()
elif mode == "connect":
    try:
        c = socket.create_connection((inputs["host"], inputs["port"]), timeout=2)
        c.close()
        result([{"fields": {"connected": True}}])
    except OSError as e:
        result([{"fields": {"connected": False, "error": type(e).__name__}}])
elif mode == "readfile":
    try:
        with open(inputs["path"], "rb") as fh:
            result([{"fields": {"readable": True, "head": fh.read(64).decode("utf-8", "replace")}}])
    except OSError as e:
        result([{"fields": {"readable": False, "error": type(e).__name__}}])
elif mode == "flaky":  # crash on the first attempt, succeed afterwards
    marker = inputs["marker"]
    if not os.path.exists(marker):
        open(marker, "w").close()
        os.kill(os.getpid(), signal.SIGSEGV)
    result([{"fields": {"recovered": True}}])
elif mode == "count_crash":  # always crash, leaving one line per attempt
    with open(inputs["attempts"], "a") as f:
        f.write("x\n")
    os.kill(os.getpid(), signal.SIGSEGV)
elif mode == "slow_first":  # hang on the first attempt, finish on the second
    marker = inputs["marker"]
    if not os.path.exists(marker):
        open(marker, "w").close()
        spawn_tree(inputs["pidfile"])
        time.sleep(300)
    result([{"fields": {"attempt": "second"}}])
elif mode == "tunnel":
    # speak HTTP CONNECT to the per-run proxy named in the environment
    from urllib.parse import urlparse
    proxy = urlparse(os.environ["HTTPS_PROXY"])
    s = socket.create_connection((proxy.hostname, proxy.port), timeout=5)
    s.settimeout(5)
    target = inputs["target"]
    cred = inputs.get("credentials", f"{proxy.username}:{proxy.password}")
    head = f"CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n"
    if cred:
        head += "Proxy-Authorization: Basic " + base64.b64encode(cred.encode()).decode() + "\r\n"
    s.sendall((head + "\r\n").encode())
    resp = b""
    while b"\r\n\r\n" not in resp:
        chunk = s.recv(4096)
        if not chunk:
            break
        resp += chunk
    status = int(resp.split(b" ", 2)[1]) if resp.startswith(b"HTTP/") else 0
    echo = ""
    if status == 200:
        s.sendall(b"ping-through-tunnel")
        echo = s.recv(64).decode()
    result([{"fields": {"status": status, "echo": echo, "header": resp.split(b"\r\n\r\n")[0].decode(),
                        "proxy_url": init.get("proxy", {}).get("url"), "env_https": os.environ.get("HTTPS_PROXY"),
                        "env_http": os.environ.get("HTTP_PROXY")}}])
elif mode == "plain_proxy":
    from urllib.parse import urlparse
    proxy = urlparse(os.environ["HTTP_PROXY"])
    s = socket.create_connection((proxy.hostname, proxy.port), timeout=5)
    cred = base64.b64encode(f"{proxy.username}:{proxy.password}".encode()).decode()
    s.sendall(f"GET http://127.0.0.1:1/ HTTP/1.1\r\nHost: 127.0.0.1:1\r\nProxy-Authorization: Basic {cred}\r\n\r\n".encode())
    result([{"fields": {"status": int(s.recv(4096).split(b" ", 2)[1])}}])
elif mode == "noproxy":
    result([{"fields": {"proxy": init.get("proxy"), "https": os.environ.get("HTTPS_PROXY"), "http": os.environ.get("HTTP_PROXY")}}])
elif mode == "peek":
    # report whether this plugin can read other plugins' /proc entries
    seen = {}
    for pid in os.listdir("/proc"):
        if not pid.isdigit() or int(pid) == os.getpid():
            continue
        try:
            with open(f"/proc/{pid}/cmdline", "rb") as fh:
                argv = fh.read().decode(errors="replace").split("\0")
        except OSError:
            continue
        for needle in inputs["needles"]:
            # a python process running that script (not any process mentioning it)
            if "python" in os.path.basename(argv[0]) and len(argv) > 1 and argv[1].endswith(needle):
                try:
                    with open(f"/proc/{pid}/environ", "rb") as fh:
                        fh.read(16)
                    seen.setdefault(needle, []).append("readable")
                except OSError as e:
                    seen.setdefault(needle, []).append(type(e).__name__)
    result([{"fields": {"seen": seen}}])
elif mode == "stall":
    # ask for many large responses and never read any of them
    for i in range(40):
        send({"type": "fetch", "id": i + 1, "url": inputs["url"]})
    time.sleep(300)
elif mode == "hold_slot":
    # used by the capacity tests: announce, hold the slot, then finish
    time.sleep(float(inputs.get("seconds", 0.3)))
    result([{"fields": {"pid": os.getpid()}}])
else:
    send({"type": "failure", "code": "invalid_input", "message": "unknown mode " + mode})
