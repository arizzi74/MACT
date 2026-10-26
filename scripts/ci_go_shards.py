#!/usr/bin/env python3
"""Discover complete Go suites, partition registry tests, and audit JSON results.

Discovery uses the compiled Go test binary, so build constraints, external tests,
executable examples and fuzz seed suites follow exactly the local Go toolchain.
Sorted round robin balances test counts without a hand-maintained allowlist.
"""
import argparse
import collections
import json
import pathlib
import re
import subprocess
import sys


REGISTRY = './internal/registry'
LIST_PATTERN = '^(Test|Example|Fuzz)'


def go_output(*args):
    return subprocess.run(['go', *args], check=True, text=True, stdout=subprocess.PIPE).stdout


def parse_packages(output):
    packages = output.splitlines()
    if not packages or any(not name or name.strip() != name or any(c.isspace() for c in name)
                           for name in packages):
        raise ValueError('Go package discovery returned an empty or invalid package list')
    if len(packages) != len(set(packages)):
        raise ValueError('Go package discovery returned duplicate packages')
    return sorted(packages)


def parse_discovery(output, package):
    names = []
    footers = 0
    for line in output.splitlines():
        if line.startswith(('Test', 'Example', 'Fuzz')) and line.isidentifier():
            names.append(line)
        elif re.fullmatch(r'ok\s+' + re.escape(package) + r'\s+\d+(?:\.\d+)?s', line):
            footers += 1
        else:
            raise ValueError(f'unrecognized Go test discovery output: {line!r}')
    if footers != 1 or not names:
        raise ValueError('Go test discovery must return runnable tests and one successful package result')
    if len(names) != len(set(names)):
        raise ValueError('Go test discovery returned duplicate test names')
    return sorted(names)


def partition(names, count):
    if count < 1 or count > len(names):
        raise ValueError('shard count must be positive and must not create empty shards')
    if len(names) != len(set(names)):
        raise ValueError('cannot partition duplicate test names')
    ordered = sorted(names)
    shards = [ordered[index::count] for index in range(count)]
    assigned = [name for shard in shards for name in shard]
    if collections.Counter(assigned) != collections.Counter(names):
        raise ValueError('shard plan omitted or duplicated discovered tests')
    return shards


def run_pattern(names):
    if not names or any(not name.isidentifier() for name in names):
        raise ValueError('cannot select an empty or invalid test suite')
    # Go identifiers contain no regexp metacharacters. Anchor only the top-level
    # name: Go then runs every subtest and every seed below the matched function.
    return '^(' + '|'.join(names) + ')$'


def save_manifest(path, manifest):
    destination = pathlib.Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(manifest, indent=2) + '\n')


def registry_plan(shard, count, path):
    packages = parse_packages(go_output('list', '-race', REGISTRY))
    if len(packages) != 1:
        raise ValueError('registry discovery must identify exactly one package')
    package = packages[0]
    names = parse_discovery(go_output('test', '-race', '-count=1', '-list', LIST_PATTERN,
                                     REGISTRY), package)
    shards = partition(names, count)
    if not 0 <= shard < count:
        raise ValueError(f'shard index must be between 0 and {count - 1}')
    selected = shards[shard]
    save_manifest(path, {'packages': packages, 'all_names': names, 'shards': shards,
                         'shard': shard, 'selected_names': selected})
    print(f'Registry shard {shard + 1}/{count}: {len(selected)}/{len(names)} top-level '
          f'tests, examples and fuzz suites; shard sizes: {[len(s) for s in shards]}', file=sys.stderr)
    for name in selected:
        print(f'  {name}', file=sys.stderr)
    return run_pattern(selected)


def other_packages(path):
    # Match the test build constraints too: packages containing only race-tagged
    # source files must not disappear from the exhaustive package selection.
    packages = parse_packages(go_output('list', '-race', './...'))
    registry = parse_packages(go_output('list', '-race', REGISTRY))
    if len(registry) != 1 or registry[0] not in packages:
        raise ValueError('full package discovery is missing the registry package')
    selected = [package for package in packages if package != registry[0]]
    if not selected:
        raise ValueError('no packages remain outside the registry shards')
    save_manifest(path, {'packages': selected, 'excluded_registry': registry[0]})
    print(f'Race testing all {len(selected)} packages outside the registry shards', file=sys.stderr)
    return '\n'.join(selected)


def audit_results(events, manifest=None):
    starts = collections.Counter()
    package_starts = collections.Counter()
    endings = {}
    package_endings = {}
    timings = []
    for event in events:
        action, package, test = event.get('Action'), event.get('Package'), event.get('Test')
        if not test:
            if action == 'start':
                package_starts[package] += 1
            elif action in ('pass', 'fail', 'skip'):
                package_endings[package] = action
            continue
        key = (package, test)
        if action == 'run':
            starts[key] += 1
        elif action in ('pass', 'fail', 'skip'):
            endings[key] = action
            if isinstance(event.get('Elapsed'), (int, float)):
                timings.append((event['Elapsed'], package, test))
    errors = []
    if not package_starts:
        errors.append('Go JSON log contains no package starts')
    if manifest:
        expected_packages = set(manifest['packages'])
        if set(package_starts) != expected_packages:
            errors.append('executed package set differs from the discovered package set')
        if any(count != 1 for count in package_starts.values()):
            errors.append('a discovered package ran more than once')
        if 'selected_names' in manifest:
            package = manifest['packages'][0]
            expected = set(manifest['selected_names'])
            actual = {test for pkg, test in starts if pkg == package and '/' not in test}
            if expected != actual:
                errors.append(f'registry execution differs from discovery; missing={sorted(expected - actual)}, '
                              f'unexpected={sorted(actual - expected)}')
            if any(starts[(package, test)] != 1 for test in expected):
                errors.append('a selected registry test did not run exactly once')
            skipped = sorted(test for (pkg, test), action in endings.items()
                             if pkg == package and action == 'skip')
            if skipped:
                errors.append(f'registry suite skipped tests: {skipped}')
    for key in starts:
        if key not in endings:
            errors.append(f'test has no terminal result: {key[0]} {key[1]}')
    for package in package_starts:
        if package not in package_endings:
            errors.append(f'package has no terminal result: {package}')
    if 'fail' in endings.values() or 'fail' in package_endings.values():
        errors.append('Go reported failing tests or packages')
    return {'starts': starts, 'endings': endings, 'packages': package_endings,
            'timings': sorted(timings, reverse=True), 'errors': errors}


def summarize(log, manifest_path, label, elapsed_seconds=None):
    events = []
    with pathlib.Path(log).open() as source:
        for line in source:
            event = json.loads(line)
            if not isinstance(event, dict):
                raise ValueError('Go JSON log contains a non-object event')
            events.append(event)
    manifest = json.loads(pathlib.Path(manifest_path).read_text()) if manifest_path else None
    result = audit_results(events, manifest)
    counts = collections.Counter(result['endings'].values())
    top_level = sum('/' not in test for _, test in result['starts'])
    lines = [f'### {label}', '',
             f'{len(result["packages"])} packages; {top_level} top-level tests/examples/fuzz suites; '
             f'{counts["pass"]} passed, {counts["skip"]} skipped, {counts["fail"]} failed '
             '(including subtests).']
    if elapsed_seconds is not None:
        lines.extend(['', f'Elapsed wall time: {elapsed_seconds}s '
                      '(discovery, compilation and tests).'])
    lines.extend(['', 'Slowest tests:', ''])
    for elapsed, package, test in result['timings'][:10]:
        lines.append(f'- {elapsed:.3f}s `{package}: {test}`')
    skipped = [(package, test) for (package, test), action in result['endings'].items() if action == 'skip']
    if skipped:
        lines.extend(['', 'Skipped tests (native/environment requirements remain visible):', ''])
        lines.extend(f'- `{package}: {test}`' for package, test in sorted(skipped))
    for error in result['errors']:
        lines.append(f'\nERROR: {error}')
    text = '\n'.join(lines) + '\n'
    print(text)
    return text, not result['errors']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    registry = commands.add_parser('registry')
    registry.add_argument('--shard', type=int, required=True)
    registry.add_argument('--shards', type=int, required=True)
    registry.add_argument('--manifest', required=True)
    packages = commands.add_parser('packages')
    packages.add_argument('--manifest', required=True)
    summary = commands.add_parser('summary')
    summary.add_argument('--log', required=True)
    summary.add_argument('--manifest')
    summary.add_argument('--label', required=True)
    summary.add_argument('--elapsed-seconds', type=int)
    summary.add_argument('--github-summary')
    args = parser.parse_args()
    try:
        if args.command == 'registry':
            print(registry_plan(args.shard, args.shards, args.manifest))
        elif args.command == 'packages':
            print(other_packages(args.manifest))
        else:
            text, success = summarize(args.log, args.manifest, args.label, args.elapsed_seconds)
            if args.github_summary:
                with pathlib.Path(args.github_summary).open('a') as destination:
                    destination.write(text)
            return 0 if success else 1
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f'Go test planning/audit failed: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
