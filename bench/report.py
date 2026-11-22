#!/usr/bin/env python3
"""Summarize complete, comparable matrix runs without averaging percentiles together."""
import argparse
from collections import defaultdict
import json
from statistics import median


from matrix import APPS, DEFAULT_APPS


def report(paths, apps):
    groups = defaultdict(list)
    profile = None
    trials = set()
    for path in paths:
        with open(path) as inp:
            for line in inp:
                row = json.loads(line)
                mode = row['mode']
                if mode not in ('load', 'memory'):
                    raise ValueError(f'{path}: expected load or memory results')
                settings = row['load']['settings']
                # The ephemeral listening port is not part of the workload.
                settings = {k: v for k, v in settings.items() if k != 'base_url'}
                affinity = row['cpu_affinity'].copy()
                db_cpu_key = 'mariadb' if row['database'] == 'mariadb' else 'mysql'
                affinity['database'] = affinity.pop(db_cpu_key)
                current = (mode, settings, affinity, row.get('sample_interval_ms'),
                           row.get('idle_seconds'), row.get('after_sync_seconds'))
                if profile is not None and current != profile:
                    raise ValueError('cannot mix load profiles or affinity settings')
                profile = current
                key = row['app'], row['database']
                stamp = (*key, row['trial'])
                if stamp in trials:
                    raise ValueError(f'duplicate trial {stamp}')
                trials.add(stamp)
                groups[key].append(row)
    expected = {(app, db) for app in apps for db in (('mariadb',) if app == 'reference' else ('sqlite', 'mysql'))}
    if set(groups) != expected:
        raise ValueError(f'unexpected or missing app/database pairs: {sorted(expected ^ set(groups))} (select with --apps)')
    if len({tuple(sorted(r['trial'] for r in rows)) for rows in groups.values()}) != 1:
        raise ValueError('different sets of trials')
    if profile[0] == 'memory':
        print('Process RSS in MiB, median across trials. Sync peaks are sampled, not lifetime high-water marks.')
        print('| App | DB | App idle | App sync peak | App after sync | DB idle | DB sync peak | DB after sync |')
        print('|---|---|---:|---:|---:|---:|---:|---:|')
        for (app, db), rows in sorted(groups.items()):
            cells = [app, db]
            for name in ('app', db):
                for phase in ('idle', 'sync_sampled_peak', 'after_sync'):
                    if name == 'sqlite':
                        cells.append('—')
                    else:
                        cells.append(f'{median(r["rss_kib"][phase][name] for r in rows) / 1024:.1f}')
            print('| ' + ' | '.join(cells) + ' |')
        return
    print('Operations/s and p95 milliseconds: median of independent trial statistics, not pooled p95.')
    print('| App | DB | History changed ops/s · p95 ms | History replay ops/s · p95 ms | Favourites GET+POST ops/s · p95 ms | App peak RSS MiB | DB peak RSS MiB |')
    print('|---|---|---:|---:|---:|---:|---:|')
    for (app, db), rows in sorted(groups.items()):
        cells = [app, db]
        for name in ('history_changed', 'history_replay', 'favourites_exchange'):
            operations = [r['load']['operations'][name] for r in rows]
            rps = median(o['throughput_ops_per_sec'] for o in operations)
            p95 = median(o['p95_ms'] for o in operations)
            cells.append(f'{rps:.1f} · {p95:.1f}')
        mem = [r['sampled_peak_rss_kib'] for r in rows]
        cells.extend(f'{median(m[p] for m in mem) / 1024:.0f}' if p != 'sqlite' else '—' for p in ('app', db))
        print('| ' + ' | '.join(cells) + ' |')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('files', nargs='+')
    parser.add_argument('--apps', nargs='+', choices=[*APPS, 'reference'], default=DEFAULT_APPS)
    args = parser.parse_args()
    report(args.files, args.apps)
