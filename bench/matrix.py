#!/usr/bin/env python3
"""Run one matrix cell at a time on disposable storage, never against a supplied URL/DB."""
import argparse
from contextlib import contextmanager, ExitStack
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import secrets
import select
import signal
import socket
import subprocess as sp
import sys
import tempfile
import threading
import time
from urllib.request import ProxyHandler, build_opener

from endpoints import check as check_endpoints

ROOT = Path(__file__).resolve().parents[1]
BUILD = ROOT / 'build/bench'
APPS = {'axum': BUILD / 'axum', 'crystal': BUILD / 'crystal', 'go': BUILD / 'go', 'csharp': BUILD / 'csharp/Kotatsu',
        'kotlin': ROOT / 'apps/kotlin/build/install/kotatsu-kotlin/bin/kotatsu-kotlin'}
DEFAULT_APPS = tuple(APPS)
APPS.update({
    'csharp-aot': BUILD / 'csharp-aot/Kotatsu',
    'kotlin-aot': BUILD / 'kotlin-aot/kotatsu-kotlin',
    'kotlin-cio': BUILD / 'kotlin-cio/bin/kotatsu-kotlin-cio',
    'kotlin-cio-aot': BUILD / 'kotlin-cio-aot/kotatsu-kotlin',
})
MYSQL = os.getenv('MYSQL_IMAGE', 'docker.io/library/mysql:8.4')
MAIL = os.getenv('MAILPIT_IMAGE', 'docker.io/axllent/mailpit:latest')
HTTP = build_opener(ProxyHandler({}))


def text(*cmd, **kwargs):
    return sp.check_output(cmd, text=True, **kwargs).strip()


@contextmanager
def container(image, args, server_args=()):
    cid = text('podman', 'create', '--pull=never', *args, image, *server_args)
    try:
        sp.run(['podman', 'start', cid], stdout=sp.DEVNULL, check=True)
        yield cid
    finally:
        sp.run(['podman', 'rm', '-fv', cid], check=True, stdout=sp.DEVNULL)


def published(cid, port):
    host, port = text('podman', 'port', cid, f'{port}/tcp').split(':')
    if host != '127.0.0.1':
        raise ValueError('fixture port is not loopback-only')
    return int(port)


def fresh_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def ready(fn, description, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if fn():
            return
        time.sleep(0.25)
    raise TimeoutError(f'{description} did not become ready')


def http_ok(url):
    try:
        with HTTP.open(url, timeout=2) as response:
            return response.status == 200
    except OSError:
        return False


def pinned(cpus, cmd):
    return ['taskset', '-c', cpus, *cmd] if cpus else cmd


@contextmanager
def application(target, work, env, cpus):
    with (work / 'server.log').open('wb') as log:
        proc = sp.Popen(pinned(cpus, [APPS[target]]), cwd=work, env=env,
                        stdout=log, stderr=sp.STDOUT, start_new_session=True)
        try:
            def healthy():
                if proc.poll() is not None:
                    raise RuntimeError(f'server exited with {proc.returncode}')
                return http_ok(env['BASE_URL'] + '/')
            ready(healthy, target)
            yield proc
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGTERM)
                try:
                    proc.wait(timeout=10)
                except sp.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait()


def rss_kib(pids):
    result = {}
    for name, pid in pids.items():
        lines = Path(f'/proc/{pid}/status').read_text().splitlines()
        result[name] = next(int(s.split()[1]) for s in lines if s.startswith('VmRSS:'))
    return result


class PeakRSS:
    """Sample Linux process RSS (KiB), not heap or cgroup memory."""
    def __init__(self, pids):
        self.peaks = {name: 0 for name in pids}
        self.pids = pids
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.sample)
        self.error = None

    def sample(self):
        try:
            while True:
                for name, value in rss_kib(self.pids).items():
                    self.peaks[name] = max(value, self.peaks[name])
                if self.stop.wait(0.1):
                    break
        except Exception as exc:
            self.error = exc

    def __enter__(self):
        self.thread.start()
        return self.peaks

    def __exit__(self, *_):
        self.stop.set()
        self.thread.join()
        if self.error is not None:
            raise RuntimeError('process disappeared during sampling') from self.error


def load_command(args, base):
    cmd = [BUILD / 'load', '--base-url', base]
    for key in ('clients', 'library', 'rounds', 'warmup'):
        cmd.extend(['--' + key, str(getattr(args, key))])
    if args.neighbour_library:
        cmd.extend(['--neighbour-library', str(args.neighbour_library)])
    return pinned(args.client_cpus, cmd)


def measure_memory(args, target, work, env, database_pids):
    cmd = load_command(args, env['BASE_URL']) + ['--pause-after-seed']
    with ExitStack() as cleanup:
        with application(target, work, env, args.app_cpus):
            client = cleanup.enter_context(sp.Popen(cmd, stdin=sp.PIPE, stdout=sp.PIPE, text=True))
            cleanup.callback(lambda: client.kill() if client.poll() is None else None)
            if not select.select([client.stdout], [], [], 120)[0] or client.stdout.readline() != 'SEEDED\n':
                raise RuntimeError('client did not confirm its seeded and verified fixture')
        # Only the Go load generator and, for MySQL, mysqld survive the restart.
        with application(target, work, env, args.app_cpus) as app:
            pids = dict(database_pids, app=app.pid)
            time.sleep(3)
            idle = rss_kib(pids)
            with PeakRSS(pids) as peak:
                output, _ = client.communicate(input='\n', timeout=300)
                if client.returncode:
                    raise RuntimeError(f'load client exited with status {client.returncode}')
            load = json.loads(output)
            time.sleep(10)
            after = rss_kib(pids)
            return {'load': load, 'rss_kib': {'idle': idle, 'sync_sampled_peak': peak, 'after_sync': after},
                    'idle_seconds': 3, 'after_sync_seconds': 10, 'sample_interval_ms': 100}


def run_cell(args, target, dialect, trial, work):
    port = fresh_port()
    base = f'http://127.0.0.1:{port}'
    env = dict(os.environ, PORT=str(port), BASE_URL=base, JWT_SECRET=secrets.token_hex(32),
               ALLOW_NEW_REGISTER='true', AUTH_RATE_LIMIT='1000', MAIL_PROVIDER='console',
               DATABASE_DIALECT=dialect, DATABASE_PATH=str(work / 'kotatsu.sqlite'))
    record = {'app': target, 'database': dialect, 'trial': trial, 'mode': args.mode,
              'cpu_affinity': {'app': args.app_cpus, 'client': args.client_cpus, 'mysql': args.mysql_cpus},
              'host': platform.uname().machine, 'kernel': platform.release()}
    if target.endswith('-aot'):
        with APPS[target].open('rb') as executable:
            record['executable_sha256'] = hashlib.file_digest(executable, 'sha256').hexdigest()
    with ExitStack() as stack:
        pids = {}
        if dialect == 'mysql':
            password = secrets.token_hex(24)
            admin_password = secrets.token_hex(24)
            config = ['-p', '127.0.0.1::3306', '-e', 'MYSQL_ROOT_PASSWORD=' + admin_password, '-e',
                      'MYSQL_DATABASE=kotatsu', '-e', 'MYSQL_USER=kotatsu', '-e', 'MYSQL_PASSWORD=' + password]
            cid = stack.enter_context(container(MYSQL, config))
            env.update(DATABASE_HOST='127.0.0.1', DATABASE_PORT=str(published(cid, 3306)),
                       DATABASE_NAME='kotatsu', DATABASE_USER='kotatsu', DATABASE_PASSWORD=password)
            # Never warm the application's caching_sha2_password entry with
            # the CLI. Its first real connection must handle full authentication.
            sql = ['podman', 'exec', '-e', 'MYSQL_PWD=' + admin_password, cid, 'mysql', '--protocol=tcp',
                   '-h127.0.0.1', '-uroot', '-N', 'kotatsu', '-e']
            ready(lambda: sp.run([*sql, 'SELECT 1'], stdout=sp.DEVNULL, stderr=sp.DEVNULL).returncode == 0,
                  'MySQL', 120)
            record['mysql_version'], flush, sync_binlog = text(*sql,
                'SELECT VERSION(), @@innodb_flush_log_at_trx_commit, @@sync_binlog').split()
            if flush != '1' or sync_binlog != '1':
                raise RuntimeError('MySQL is not configured for durable commits')
            record['mysql_image'] = text('podman', 'image', 'inspect', MYSQL, '--format', '{{.Digest}}')
            pids['mysql'] = int(text('podman', 'inspect', cid, '--format', '{{.State.Pid}}'))
            if args.mysql_cpus:
                # Rootless cgroups need not delegate cpuset; set the scheduler
                # affinity of every mysqld thread before any fixture connects.
                sp.run(['podman', 'unshare', 'taskset', '-apc', args.mysql_cpus, str(pids['mysql'])],
                       check=True, stdout=sp.DEVNULL)
        if args.mode == 'verify':
            mail = stack.enter_context(container(MAIL, ['-p', '127.0.0.1::8025', '-p', '127.0.0.1::1025']))
            mail_url = f'http://127.0.0.1:{published(mail, 8025)}'
            env.update(MAIL_PROVIDER='smtp', SMTP_HOST='127.0.0.1', SMTP_PORT=str(published(mail, 1025)),
                       SMTP_USERNAME='', SMTP_PASSWORD='', SMTP_FROM='kotatsu@example.invalid')
            ready(lambda: http_ok(mail_url + '/api/v1/messages'), 'Mailpit')
        if args.mode == 'memory':
            record.update(measure_memory(args, target, work, env, pids))
            return record
        app = stack.enter_context(application(target, work, env, args.app_cpus))
        if args.mode == 'verify':
            results = []
            suites = ['sync', 'history-policy', 'recovery']
            if target == 'axum':
                suites.append('favourites-policy')
            for suite in suites:
                cmd = [BUILD / 'contract', 'verify', '--base-url', base, '--suite', suite]
                if suite == 'recovery':
                    cmd.extend(['--mailpit-url', mail_url])
                results.append(text(*cmd, cwd=ROOT / 'contract-tests'))
            check_endpoints(port)
            record['checks'] = results + ['public endpoints verified']
        else:
            with PeakRSS(dict(pids, app=app.pid)) as peaks:
                record['load'] = json.loads(text(*load_command(args, base)))
            if not all(peaks.values()):
                raise RuntimeError('could not sample the server process')
            record['sampled_peak_rss_kib'] = peaks
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['verify', 'load', 'memory'])
    parser.add_argument('--apps', nargs='+', choices=APPS, default=DEFAULT_APPS)
    parser.add_argument('--databases', nargs='+', choices=['sqlite', 'mysql'], default=['sqlite', 'mysql'])
    parser.add_argument('--trials', type=int, default=1)
    parser.add_argument('--clients', type=int, default=4)
    parser.add_argument('--library', type=int, default=50)
    parser.add_argument('--rounds', type=int, default=10)
    parser.add_argument('--warmup', type=int, default=3)
    parser.add_argument('--neighbour-library', type=int, default=0,
                        help='also measure small updates during a fresh import of this many rows')
    parser.add_argument('--app-cpus')
    parser.add_argument('--mysql-cpus')
    parser.add_argument('--client-cpus')
    parser.add_argument('--output', required=True, type=Path, help='new JSONL output, never overwritten')
    args = parser.parse_args()
    if args.trials < 1:
        parser.error('--trials must be positive')
    if args.mode == 'memory' and args.neighbour_library:
        parser.error('memory mode excludes the neighbouring account registration')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    BUILD.mkdir(parents=True, exist_ok=True)
    with args.output.open('x') as output:
        for trial in range(1, args.trials + 1):
            cells = [(app, db) for app in args.apps for db in args.databases]
            random.Random(100 + trial).shuffle(cells)
            for target, dialect in cells:
                print(f'{args.mode} {target}/{dialect} trial {trial}', file=sys.stderr, flush=True)
                with tempfile.TemporaryDirectory(dir=BUILD, prefix='fixture-') as tmp:
                    work = Path(tmp)
                    try:
                        record = run_cell(args, target, dialect, trial, work)
                    except Exception:
                        # Logs may contain credentials. Do not publish them in result files.
                        if (work / 'server.log').exists():
                            with tempfile.NamedTemporaryFile(dir=BUILD, prefix='failed-', suffix='.log', delete=False) as log:
                                log.write((work / 'server.log').read_bytes())
                                print('Private diagnostic log:', log.name, file=sys.stderr)
                        raise
                    print(json.dumps(record, separators=(',', ':')), file=output, flush=True)


if __name__ == '__main__':
    main()
