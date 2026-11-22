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
RATE_DEFAULTS = {
    'RATE_LIMIT_API_PER_SECOND': '200',
    'RATE_LIMIT_AUTH_PER_MINUTE': '5',
    'RATE_LIMIT_FORGOT_PER_10_MINUTES': '5',
    'RATE_LIMIT_RESET_PER_10_MINUTES': '5',
    'RATE_LIMIT_PASSWORD_CONCURRENCY': '2',
}
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


def cpu_ticks(pids):
    # comm is parenthesized and may contain spaces or parentheses; fields
    # following its last ')' start with field 3 (state).
    result = {}
    for name, pid in pids.items():
        fields = Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()
        result[name] = int(fields[11]) + int(fields[12])
    return result


class PeakRSS:
    """Sample Linux process RSS (KiB), not heap or cgroup memory."""
    def __init__(self, pids, series_start=None):
        self.peaks = {name: 0 for name in pids}
        self.pids = pids
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.sample)
        self.error = None
        self.series_start = series_start
        self.series = []

    def sample(self):
        try:
            while True:
                values = rss_kib(self.pids)
                for name, value in values.items():
                    self.peaks[name] = max(value, self.peaks[name])
                if self.series_start is not None:
                    elapsed = time.monotonic() - self.series_start
                    if not self.series or elapsed - self.series[-1]['elapsed_ms'] / 1000 >= 1:
                        self.series.append({'elapsed_ms': elapsed * 1000, 'rss_kib': values})
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


def measure_memory(args, target, work, env, database_pids, *, process_metrics=False):
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
            if process_metrics:
                before_cpu = cpu_ticks(pids)
                started = time.monotonic()
            sampler = PeakRSS(pids, started if process_metrics else None)
            with sampler as peak:
                output, _ = client.communicate(input='\n', timeout=300)
                if client.returncode:
                    raise RuntimeError(f'load client exited with status {client.returncode}')
                if process_metrics:
                    after_cpu = cpu_ticks(pids)
                    duration_ms = (time.monotonic() - started) * 1000
            load = json.loads(output)
            time.sleep(10)
            after = rss_kib(pids)
            result = {'load': load, 'rss_kib': {'idle': idle, 'sync_sampled_peak': peak, 'after_sync': after},
                      'idle_seconds': 3, 'after_sync_seconds': 10, 'sample_interval_ms': 100}
            if process_metrics:
                ticks_per_second = os.sysconf('SC_CLK_TCK')
                result['process_metrics'] = {
                    'interval': 'post-restart client resume through final output; includes warmup and readbacks, excludes registration',
                    'duration_ms': duration_ms,
                    'cpu_ms': {name: (after_cpu[name] - ticks) * 1000 / ticks_per_second
                               for name, ticks in before_cpu.items()},
                    'rss_sample_interval_ms': 1000,
                    'rss_series': [sample for sample in sampler.series if sample['elapsed_ms'] <= duration_ms],
                }
            return result


def run_cell(args, target, dialect, trial, work):
    port = fresh_port()
    base = f'http://127.0.0.1:{port}'
    env = dict(os.environ, PORT=str(port), BASE_URL=base, JWT_SECRET=secrets.token_hex(32),
               ALLOW_NEW_REGISTER='true', MAIL_PROVIDER='console',
               DATABASE_DIALECT=dialect, DATABASE_PATH=str(work / 'kotatsu.sqlite'))
    for key in RATE_DEFAULTS:
        env.pop(key, None)
    rates = dict(RATE_DEFAULTS)
    if args.mode != 'verify':
        ceiling = os.environ.get('RATE_LIMIT_API_PER_SECOND', '1000')
        env['RATE_LIMIT_API_PER_SECOND'] = rates['RATE_LIMIT_API_PER_SECOND'] = ceiling
    record = {'app': target, 'database': dialect, 'trial': trial, 'mode': args.mode,
              'cpu_affinity': {'app': args.app_cpus, 'client': args.client_cpus, 'mysql': args.mysql_cpus},
              'host': platform.uname().machine, 'kernel': platform.release()}
    record['rate_limits'] = {key: int(value) for key, value in rates.items()}
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
            record.update(measure_memory(args, target, work, env, pids, process_metrics=args.process_metrics))
            return record
        if args.mode == 'verify':
            results = []
            suites = ['sync', 'history-policy', 'recovery']
            if target == 'axum':
                suites.append('favourites-policy')
            for suite in suites:
                cmd = [BUILD / 'contract', 'verify', '--base-url', base, '--suite', suite]
                if suite == 'recovery':
                    cmd.extend(['--mailpit-url', mail_url])
                with application(target, work, env, args.app_cpus):
                    results.append(text(*cmd, cwd=ROOT / 'contract-tests'))
            with application(target, work, env, args.app_cpus):
                check_endpoints(port)
            record['checks'] = results + ['public endpoints verified']
        else:
            app = stack.enter_context(application(target, work, env, args.app_cpus))
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
    parser.add_argument('--process-metrics', action='store_true',
                        help='memory mode: record post-restart CPU and approximately 1-second RSS samples')
    parser.add_argument('--output', required=True, type=Path, help='new JSONL output, never overwritten')
    args = parser.parse_args()
    if args.trials < 1:
        parser.error('--trials must be positive')
    if args.process_metrics and args.mode != 'memory':
        parser.error('--process-metrics is only available in memory mode')
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
