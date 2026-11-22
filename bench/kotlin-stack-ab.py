#!/usr/bin/env python3
"""Compare prebuilt Kotlin ports from two checkouts using the same matrix."""
import argparse
from hashlib import file_digest, sha256
import os
from pathlib import Path
import subprocess
import sys

import matrix


def launchers(root):
    bench = root / 'build/bench'
    return {
        'kotlin': root / 'apps/kotlin/build/install/kotatsu-kotlin/bin/kotatsu-kotlin',
        'kotlin-cio': bench / 'kotlin-cio/bin/kotatsu-kotlin-cio',
        'kotlin-aot': bench / 'kotlin-aot/kotatsu-kotlin',
        'kotlin-cio-aot': bench / 'kotlin-cio-aot/kotatsu-kotlin',
    }


def artifact_hash(launcher, native):
    if native:
        with launcher.open('rb') as binary:
            return file_digest(binary, 'sha256').hexdigest()
    # The JIT launcher is generated; the bytecode and its dependency set identify the build.
    jars = sorted((launcher.parent.parent / 'lib').glob('*.jar'))
    if not jars:
        raise FileNotFoundError(f'no runtime jars for {launcher}')
    digest = sha256()
    for jar in jars:
        with jar.open('rb') as content:
            digest.update(jar.name.encode() + b'\0' + file_digest(content, 'sha256').digest())
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('control', type=Path, help='checkout with old Kotlin/Ktor binaries')
    parser.add_argument('matrix_args', nargs=argparse.REMAINDER, help='arguments for matrix.py')
    args = parser.parse_args()
    if '--apps' in args.matrix_args:
        parser.error('this comparison always includes all four Kotlin profiles and their controls')
    java = Path(os.environ['JAVA_HOME']) / 'bin/java'
    java_version = subprocess.check_output([java, '-version'], stderr=subprocess.STDOUT, text=True).strip()
    apps = {}
    for tree, variant in ((args.control.resolve(strict=True), 'control'), (matrix.ROOT, 'candidate')):
        for name, launcher in launchers(tree).items():
            label = f'old-{name}' if variant == 'control' else name
            matrix.APPS[label] = launcher.resolve(strict=True)
            apps[label] = artifact_hash(launcher, name.endswith('-aot'))
    for name in launchers(matrix.ROOT):
        if apps[name] == apps[f'old-{name}']:
            raise ValueError(f'control and candidate for {name} are identical')

    run_cell = matrix.run_cell

    def run(args, target, dialect, trial, work):
        row = run_cell(args, target, dialect, trial, work)
        row.update(app_artifact_sha256=apps[target], java_version=java_version)
        return row

    matrix.run_cell = run
    sys.argv[1:] = [*args.matrix_args, '--apps', *apps]
    matrix.main()


if __name__ == '__main__':
    main()
