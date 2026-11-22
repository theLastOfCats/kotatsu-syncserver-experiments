#!/usr/bin/env python3
"""Measure authenticated sync with 1, 2, and 4 app CPUs in shuffled order."""
import argparse
import json
from pathlib import Path
import random
import re
import sys
import tempfile

import matrix


def cpu_set(mask):
    if not re.fullmatch(r'\d+(?:-\d+)?(?:,\d+(?:-\d+)?)*', mask):
        raise argparse.ArgumentTypeError('CPU mask must be a list of numbers or ranges')
    cpus = set()
    for part in mask.split(','):
        bounds = [int(n) for n in part.split('-')]
        if len(bounds) == 2 and bounds[0] > bounds[1]:
            raise argparse.ArgumentTypeError('CPU range must be ascending')
        cpus.update(range(bounds[0], bounds[-1] + 1))
    return cpus


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apps', nargs='+', choices=matrix.APPS, default=tuple(matrix.APPS))
    parser.add_argument('--databases', nargs='+', choices=['sqlite', 'mysql'], default=['sqlite', 'mysql'])
    parser.add_argument('--trials', type=int, default=3)
    parser.add_argument('--library', type=int, default=200, help='history and favourite entries per client')
    parser.add_argument('--rounds', type=int, default=100, help='timed operations per client per phase')
    parser.add_argument('--warmup', type=int, default=25, help='warmup operations per client per phase')
    parser.add_argument('--app-cpus', nargs=3, default=['0', '0-1', '0-3'], metavar=('ONE', 'TWO', 'FOUR'))
    parser.add_argument('--mysql-cpus', default='4-5')
    parser.add_argument('--client-cpus', default='6-7')
    parser.add_argument('--output', required=True, type=Path, help='new JSONL output, never overwritten')
    args = parser.parse_args()
    if min(args.trials, args.library, args.rounds) < 1 or args.warmup < 0:
        parser.error('trials, library and rounds must be positive; warmup cannot be negative')
    if len(set(args.apps)) != len(args.apps) or len(set(args.databases)) != len(args.databases):
        parser.error('select each app and database only once')
    try:
        masks = [cpu_set(mask) for mask in args.app_cpus]
        mysql_cpus, client_cpus = cpu_set(args.mysql_cpus), cpu_set(args.client_cpus)
    except argparse.ArgumentTypeError as exc:
        parser.error(str(exc))
    if [len(cpus) for cpus in masks] != [1, 2, 4]:
        parser.error('--app-cpus must specify masks containing 1, 2, and 4 CPUs respectively')
    if not masks[0] < masks[1] < masks[2]:
        parser.error('app CPU masks must be nested')
    if mysql_cpus & client_cpus or any(cpus & (mysql_cpus | client_cpus) for cpus in masks):
        parser.error('app, MySQL, and client CPU masks must be disjoint')

    args.mode = 'memory'
    args.process_metrics = True
    args.clients = 4
    args.neighbour_library = 0
    app_masks = list(zip(args.app_cpus, (1, 2, 4)))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    matrix.BUILD.mkdir(parents=True, exist_ok=True)
    with args.output.open('x') as output:
        for trial in range(1, args.trials + 1):
            cells = [(app, db, mask, cores) for app in args.apps for db in args.databases
                     for mask, cores in app_masks]
            random.Random(100 + trial).shuffle(cells)
            for target, dialect, mask, cores in cells:
                args.app_cpus = mask
                print(f'memory {target}/{dialect} app CPUs {mask} trial {trial}', file=sys.stderr, flush=True)
                with tempfile.TemporaryDirectory(dir=matrix.BUILD, prefix='fixture-') as tmp:
                    work = Path(tmp)
                    try:
                        record = matrix.run_cell(args, target, dialect, trial, work)
                    except Exception:
                        # Logs may contain credentials; keep them private and out of JSONL.
                        if (work / 'server.log').exists():
                            with tempfile.NamedTemporaryFile(dir=matrix.BUILD, prefix='failed-', suffix='.log',
                                                             delete=False) as log:
                                log.write((work / 'server.log').read_bytes())
                                print('Private diagnostic log:', log.name, file=sys.stderr)
                        raise
                    record['app_cores'] = cores
                    print(json.dumps(record, separators=(',', ':')), file=output, flush=True)


if __name__ == '__main__':
    main()
