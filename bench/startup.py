#!/usr/bin/env python3
"""Inventory runnable payloads and measure SQLite startup, one process at a time.

Logical sizes are sums of regular-file lengths, not disk blocks or compressed
archives. Whole published directories are counted (including other-platform
assets and launchers); only .pdb and .dbg debug symbols are excluded. Symlinks
are recorded as links, not followed. The deterministic tree hash covers sorted
relative paths, file contents and link targets, not timestamps or permissions.
External runtimes and OS libraries are not part of the application payload.

Each randomized cell starts with a nonexistent SQLite file, then immediately
restarts the process on that empty, initialized database. Neither is a warm
JVM. No cache flush is attempted: the host page cache and filesystem state are
uncontrolled. Readiness is the first observed HTTP 200 with body exactly Alive;
1 ms retry sleeps and request scheduling mean this is an observation, not the
exact instant the server became ready. RSS is Linux process RSS at observation,
not a peak, heap, process-tree total, or container memory limit.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import secrets
import signal
import subprocess
import sys
import tempfile
import time
from urllib.error import URLError
from urllib.request import ProxyHandler, build_opener

from matrix import APPS, BUILD, RATE_DEFAULTS, ROOT, fresh_port, pinned, rss_kib

NAMES = ('go', 'csharp', 'csharp-aot', 'kotlin', 'kotlin-aot',
         'kotlin-cio', 'kotlin-cio-aot', 'axum', 'crystal')
HTTP = build_opener(ProxyHandler({}))


def inventory(root, *, exclude_debug=False, hash_tree=True):
    """Count files without following links; hash length-framed paths and contents."""
    root = Path(root)
    if not root.exists():
        raise FileNotFoundError(root)
    entries = sorted(root.rglob('*')) if root.is_dir() else [root]
    result = {'bytes': 0, 'file_count': 0, 'symlink_count': 0,
              'excluded_debug_files': []}
    digest = hashlib.sha256() if hash_tree else None
    for path in entries:
        rel = path.relative_to(root).as_posix() if root.is_dir() else path.name
        if path.is_symlink():
            target = os.readlink(path).encode('utf-8', 'surrogateescape')
            result['symlink_count'] += 1
            if digest:
                digest.update(b'L' + len(os.fsencode(rel)).to_bytes(8, 'big') + os.fsencode(rel))
                digest.update(len(target).to_bytes(8, 'big') + target)
        elif path.is_file():
            if exclude_debug and path.suffix.lower() in ('.pdb', '.dbg'):
                result['excluded_debug_files'].append(rel)
                continue
            size = path.stat().st_size
            result['bytes'] += size
            result['file_count'] += 1
            if digest:
                name = os.fsencode(rel)
                digest.update(b'F' + len(name).to_bytes(8, 'big') + name + size.to_bytes(8, 'big'))
                with path.open('rb') as f:
                    for chunk in iter(lambda: f.read(1024 * 1024), b''):
                        digest.update(chunk)
    if digest:
        result['tree_sha256'] = digest.hexdigest()
    return result


def app_inventory(app):
    executable = APPS[app]
    if app in ('csharp', 'csharp-aot', 'kotlin-aot', 'kotlin-cio-aot'):
        root = executable.parent
    elif app in ('kotlin', 'kotlin-cio'):
        root = executable.parent.parent
    else:
        root = executable
    if not executable.is_file():
        raise FileNotFoundError(executable)
    return {'entrypoint': str(executable.relative_to(ROOT)), 'entrypoint_bytes': executable.stat().st_size,
            'payload_root': str(root.relative_to(ROOT)), 'payload': inventory(root, exclude_debug=True)}


def runtime_paths(app, pid):
    if app in ('kotlin', 'kotlin-cio'):
        executable = Path(f'/proc/{pid}/exe').resolve(strict=True)
        return {'kind': 'java_installation',
                'paths': [str(executable.parent.parent)],
                'note': 'Entire installed Java home, potentially including JDK tools; not a minimal JVM.'}
    if app == 'csharp':
        # Discover the framework versions actually mapped by the running process.
        roots = set()
        for line in Path(f'/proc/{pid}/maps').read_text().splitlines():
            fields = line.split(maxsplit=5)
            if len(fields) < 6 or not fields[5].startswith('/'):
                continue
            parts = Path(fields[5].removesuffix(' (deleted)')).parts
            for i, part in enumerate(parts):
                if ((part == 'shared' and len(parts) > i + 2 and parts[i + 1] in (
                        'Microsoft.NETCore.App', 'Microsoft.AspNetCore.App')) or
                        (part == 'host' and len(parts) > i + 2 and parts[i + 1] == 'fxr')):
                    roots.add(Path(*parts[:i + 3]))
        if not any('Microsoft.NETCore.App' in root.parts for root in roots):
            raise RuntimeError('Could not identify the running .NET shared runtime from /proc maps')
        return {'kind': 'dotnet_shared_frameworks_and_hostfxr',
                'paths': [str(root) for root in sorted(roots)],
                'note': 'Mapped framework and hostfxr version directories; excludes OS libraries.'}
    return None


def stop(proc):
    # The session may still contain children even if its leader has exited.
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        proc.wait()


def measure(app, work, port, app_cpus, timeout, runtime_cache):
    env = dict(os.environ, PORT=str(port), BASE_URL=f'http://127.0.0.1:{port}',
               JWT_SECRET=secrets.token_hex(32), ALLOW_NEW_REGISTER='true',
               MAIL_PROVIDER='console', DATABASE_DIALECT='sqlite',
               DATABASE_PATH=str(work / 'kotatsu.sqlite'))
    for key in RATE_DEFAULTS:
        env.pop(key, None)
    with (work / 'server.log').open('ab') as log:
        start = time.monotonic_ns()
        proc = subprocess.Popen(pinned(app_cpus, [str(APPS[app])]), cwd=work, env=env,
                                stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            deadline = start + int(timeout * 1_000_000_000)
            attempts = 0
            while time.monotonic_ns() < deadline:
                if proc.poll() is not None:
                    raise RuntimeError(f'{app} exited before readiness: {proc.returncode}')
                attempts += 1
                try:
                    with HTTP.open(env['BASE_URL'] + '/', timeout=0.1) as response:
                        ok = response.status == 200 and response.read() == b'Alive'
                except (OSError, URLError):
                    ok = False
                if ok:
                    elapsed = time.monotonic_ns() - start
                    rss = rss_kib({'app': proc.pid})['app']
                    if app not in runtime_cache:
                        runtime_cache[app] = runtime_paths(app, proc.pid)
                    return {'spawn_to_ready_ms': elapsed / 1_000_000,
                            'readiness_rss_kib': rss, 'http_attempts': attempts}
                time.sleep(0.001)
            raise TimeoutError(f'{app} did not become ready within {timeout} seconds')
        finally:
            stop(proc)


def cpu_set(spec):
    cpus = set()
    for item in spec.split(','):
        bounds = item.split('-')
        if len(bounds) == 1:
            cpus.add(int(bounds[0]))
        elif len(bounds) == 2 and int(bounds[0]) <= int(bounds[1]):
            cpus.update(range(int(bounds[0]), int(bounds[1]) + 1))
        else:
            raise ValueError(f'invalid CPU list: {spec}')
    return cpus


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apps', nargs='+', choices=NAMES, default=NAMES)
    parser.add_argument('--trials', type=int, default=7)
    parser.add_argument('--seed', type=int, default=100)
    parser.add_argument('--app-cpus', help='application CPU list, passed to taskset')
    parser.add_argument('--client-cpus', help='driver CPU list (Linux sched_setaffinity)')
    parser.add_argument('--timeout', type=float, default=90)
    parser.add_argument('--output', required=True, type=Path, help='new JSONL file; never overwritten')
    args = parser.parse_args()
    if args.trials < 7 or args.timeout <= 0 or len(set(args.apps)) != len(args.apps):
        parser.error('require at least 7 trials, positive timeout and unique apps')
    if args.client_cpus:
        os.sched_setaffinity(0, cpu_set(args.client_cpus))
    BUILD.mkdir(parents=True, exist_ok=True)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    runtime_cache = {}
    with args.output.open('x') as output:
        def emit(record):
            print(json.dumps(record, separators=(',', ':')), file=output, flush=True)
        emit({'type': 'metadata', 'seed': args.seed, 'trials': args.trials,
              'apps': list(args.apps), 'database': 'sqlite', 'app_cpus': args.app_cpus,
              'client_cpus': args.client_cpus,
              'machine': platform.machine(), 'kernel': platform.release(),
              'poll_sleep_ms': 1, 'request_timeout_ms': 100,
              'cache': 'uncontrolled host page cache; no privileged flush',
              'existing_database': 'empty initialized SQLite database, immediately restarted; not a warm JVM',
              'accounting': 'logical regular-file bytes; whole published tree including sidecars and other-platform assets; exclude .pdb/.dbg; symlinks recorded, not followed',
              'tree_hash': 'SHA-256 over sorted relative paths with length-framed file contents or symlink targets; excludes metadata'})
        for app in args.apps:
            emit({'type': 'inventory', 'app': app, **app_inventory(app)})
        rng = random.Random(args.seed)
        for trial in range(1, args.trials + 1):
            order = list(args.apps)
            rng.shuffle(order)
            for app in order:
                print(f'{app} trial {trial}', file=sys.stderr, flush=True)
                with tempfile.TemporaryDirectory(dir=BUILD, prefix='startup-') as tmp:
                    work = Path(tmp)
                    port = fresh_port()
                    try:
                        for state in ('new', 'existing'):
                            sample = measure(app, work, port, args.app_cpus, args.timeout, runtime_cache)
                            if not (work / 'kotatsu.sqlite').is_file():
                                raise RuntimeError(f'{app} did not create SQLite database')
                            emit({'type': 'sample', 'app': app, 'trial': trial,
                                  'database_state': state, **sample})
                    except Exception:
                        if (work / 'server.log').exists():
                            with tempfile.NamedTemporaryFile(dir=BUILD, prefix='startup-failed-', suffix='.log', delete=False) as log:
                                log.write((work / 'server.log').read_bytes())
                                print(f'Private diagnostic log: {log.name}', file=sys.stderr)
                        raise
        for app in args.apps:
            runtime = runtime_cache[app]
            if runtime is not None:
                runtime['footprints'] = [{'component': '/'.join(Path(path).parts[-2:])
                                         if app == 'csharp' else Path(path).name,
                                         'footprint': inventory(path, hash_tree=False)}
                                        for path in runtime.pop('paths')]
            emit({'type': 'runtime', 'app': app, 'external_runtime': runtime})


if __name__ == '__main__':
    main()
