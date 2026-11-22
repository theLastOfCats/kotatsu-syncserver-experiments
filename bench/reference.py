#!/usr/bin/env python3
"""Verify or benchmark the original server on a disposable MariaDB 11.8."""
import argparse
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import platform
import secrets
import shlex
import subprocess as sp
import sys
import tempfile

import matrix

IMAGE = 'docker.io/library/mariadb:11.8'


def run_trial(args, trial, work, command, artifact, suite=None):
    port = matrix.fresh_port()
    base = f'http://127.0.0.1:{port}'
    password = secrets.token_hex(24)
    root_password = secrets.token_hex(24)
    env = dict(os.environ)
    for key in ('JAVA_TOOL_OPTIONS', 'JDK_JAVA_OPTIONS', '_JAVA_OPTIONS', 'CLASSPATH'):
        env.pop(key, None)
    env.update(PORT=str(port), BASE_URL=base, JWT_SECRET=secrets.token_hex(32),
               ALLOW_NEW_REGISTER='true', MAIL_PROVIDER='console', DATABASE_DIALECT='mariadb',
               DATABASE_HOST='127.0.0.1', DATABASE_NAME='kotatsu', DATABASE_USER='kotatsu',
               DATABASE_PASSWORD=password)
    if suite:
        env.pop('KOTATSU_BENCH_UNLIMITED', None)
    else:
        env['KOTATSU_BENCH_UNLIMITED'] = '1'
    wrapper = work / 'reference'
    wrapper.write_text('#!/bin/sh\nexec ' + shlex.join([*map(str, command), '-host=127.0.0.1']) + '\n')
    wrapper.chmod(0o700)
    matrix.APPS['reference'] = wrapper
    record = {'app': 'reference', 'database': 'mariadb', 'trial': trial, 'mode': args.mode,
              'cpu_affinity': {'app': args.app_cpus, 'client': args.client_cpus, 'mariadb': args.mysql_cpus},
              'host': platform.uname().machine, 'kernel': platform.release(),
              **artifact, 'kotatsu_bench_unlimited': '0' if suite else '1'}
    with ExitStack() as stack:
        cid = stack.enter_context(matrix.container(IMAGE, [
            '-p', '127.0.0.1::3306', '-e', 'MARIADB_ROOT_PASSWORD=' + root_password,
            '-e', 'MARIADB_DATABASE=kotatsu', '-e', 'MARIADB_USER=kotatsu',
            '-e', 'MARIADB_PASSWORD=' + password]))
        env['DATABASE_PORT'] = str(matrix.published(cid, 3306))
        sql = ['podman', 'exec', '-e', 'MYSQL_PWD=' + root_password, cid, 'mariadb',
               '--protocol=tcp', '-h127.0.0.1', '-uroot', '-N', '-B', 'kotatsu', '-e']
        matrix.ready(lambda: sp.run([*sql, 'SELECT 1'], stdout=sp.DEVNULL,
                                    stderr=sp.DEVNULL).returncode == 0, 'MariaDB', 120)
        version, engine, flush, log_bin, sync_binlog = matrix.text(*sql,
            'SELECT VERSION(), @@default_storage_engine, @@innodb_flush_log_at_trx_commit, '
            '@@log_bin, @@sync_binlog').split()
        if 'mariadb' not in version.lower() or not version.startswith('11.8.'):
            raise RuntimeError('expected MariaDB 11.8, got ' + version)
        if engine.lower() != 'innodb' or flush != '1':
            raise RuntimeError('MariaDB must use InnoDB and innodb_flush_log_at_trx_commit=1')
        record.update(mariadb_version=version, mariadb_engine=engine,
                      innodb_flush_log_at_trx_commit=int(flush), log_bin=int(log_bin),
                      sync_binlog=int(sync_binlog),
                      mariadb_image=matrix.text('podman', 'image', 'inspect', IMAGE, '--format', '{{.Digest}}'))
        pid = int(matrix.text('podman', 'inspect', cid, '--format', '{{.State.Pid}}'))
        sp.run(['podman', 'unshare', 'taskset', '-apc', args.mysql_cpus, str(pid)],
               check=True, stdout=sp.DEVNULL)
        pids = {'mariadb': pid}
        if suite:
            contract = [matrix.BUILD / 'contract', 'verify', '--base-url', base, '--suite', suite]
            if suite == 'recovery':
                mail_user, mail_password = 'kotatsu', secrets.token_hex(24)
                mail = stack.enter_context(matrix.container(matrix.MAIL, [
                    '-p', '127.0.0.1::8025', '-p', '127.0.0.1::1025',
                    '-e', f'MP_SMTP_AUTH={mail_user}:{mail_password}',
                    '-e', 'MP_SMTP_AUTH_ALLOW_INSECURE=true']))
                mail_url = f'http://127.0.0.1:{matrix.published(mail, 8025)}'
                env.update(MAIL_PROVIDER='smtp', SMTP_HOST='127.0.0.1',
                           SMTP_PORT=str(matrix.published(mail, 1025)), SMTP_USERNAME=mail_user,
                           SMTP_PASSWORD=mail_password, SMTP_FROM='kotatsu@example.invalid')
                matrix.ready(lambda: matrix.http_ok(mail_url + '/api/v1/messages'), 'Mailpit')
                contract.extend(['--mailpit-url', mail_url])
            stack.enter_context(matrix.application('reference', work, env, args.app_cpus))
            record.update(suite=suite, checks=matrix.text(*contract, cwd=matrix.ROOT / 'contract-tests'))
        elif args.mode == 'memory':
            record.update(matrix.measure_memory(args, 'reference', work, env, pids))
        else:
            app = stack.enter_context(matrix.application('reference', work, env, args.app_cpus))
            with matrix.PeakRSS(dict(pids, app=app.pid)) as peaks:
                record['load'] = json.loads(matrix.text(*matrix.load_command(args, base)))
            if not all(peaks.values()):
                raise RuntimeError('could not sample the server process')
            record['sampled_peak_rss_kib'] = peaks
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('load', 'memory', 'verify'))
    executable = parser.add_mutually_exclusive_group(required=True)
    executable.add_argument('--jar', type=Path)
    executable.add_argument('--native', type=Path)
    parser.add_argument('--trace-dir', type=Path, help='collect reachability metadata in JAR verify mode')
    parser.add_argument('--clients', type=int, choices=(1, 4))
    parser.add_argument('--trials', type=int)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.mode == 'verify':
        if args.clients is not None or args.trials is not None:
            parser.error('verify runs sync and recovery once; omit --clients and --trials')
        rounds = [(1, 'sync'), (1, 'recovery')]
    else:
        if args.clients is None or args.trials is None or args.trials < 1:
            parser.error('load and memory require --clients and positive --trials')
        rounds = [(trial, None) for trial in range(1, args.trials + 1)]
    if args.trace_dir is not None and not (args.jar and args.mode == 'verify'):
        parser.error('--trace-dir requires --jar in verify mode')
    args.library, args.warmup, args.rounds = 50, 10, 50
    args.neighbour_library = 500 if args.mode == 'load' and args.clients == 4 else 0
    args.app_cpus, args.mysql_cpus, args.client_cpus = '0-3', '4-5', '6-7'
    if args.jar:
        jar = args.jar.resolve(strict=True)
        java = (Path(os.environ['JAVA_HOME']) / 'bin/java').resolve(strict=True)
        with jar.open('rb') as source:
            artifact = {'jar_sha256': hashlib.file_digest(source, 'sha256').hexdigest()}
        artifact['java_version'] = sp.check_output([java, '-version'], stderr=sp.STDOUT, text=True).strip()
        command = [java]
        if args.trace_dir:
            trace_dir = args.trace_dir.resolve(strict=True)
            command.append(f'-agentlib:native-image-agent=config-merge-dir={trace_dir}')
        command.extend(['-jar', jar])
    else:
        native = args.native.resolve(strict=True)
        with native.open('rb') as source:
            artifact = {'executable_sha256': hashlib.file_digest(source, 'sha256').hexdigest()}
        command = [native]
    matrix.BUILD.mkdir(parents=True, exist_ok=True)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open('x') as output:
        for trial, suite in rounds:
            print(f'{args.mode} reference/mariadb {suite or f"trial {trial}"}', file=sys.stderr, flush=True)
            with tempfile.TemporaryDirectory(dir=matrix.BUILD, prefix='reference-') as tmp:
                work = Path(tmp)
                try:
                    record = run_trial(args, trial, work, command, artifact, suite)
                except Exception:
                    log = work / 'server.log'
                    if log.exists():
                        with tempfile.NamedTemporaryFile(dir=matrix.BUILD, prefix='reference-failed-',
                                                         suffix='.log', delete=False) as saved:
                            saved.write(log.read_bytes())
                            print('Private diagnostic log:', saved.name, file=sys.stderr)
                    raise
            print(json.dumps(record, separators=(',', ':')), file=output, flush=True)


if __name__ == '__main__':
    main()
