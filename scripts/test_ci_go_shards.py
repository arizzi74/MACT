"""Regression tests for exhaustive CI discovery, selection and failure handling."""
import collections
import importlib.util
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('ci_go_shards', ROOT / 'scripts/ci_go_shards.py')
planner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(planner)


def event(action, test=None, package='example.test/fixture', **extra):
    result = {'Action': action, 'Package': package, **extra}
    if test is not None:
        result['Test'] = test
    return result


class DiscoveryTests(unittest.TestCase):
    def test_discovers_tests_examples_and_fuzz_suites(self):
        self.assertEqual(planner.parse_discovery(
            'TestZulu\nExample\nFuzzSeed\nTestAlpha\nok  \texample.test/fixture\t0.003s\n',
            'example.test/fixture'), ['Example', 'FuzzSeed', 'TestAlpha', 'TestZulu'])

    def test_discovery_fails_closed_on_empty_duplicate_or_unrecognized_output(self):
        for output in ('', 'ok\texample.test/fixture\t0.1s\n', 'TestOnly\n',
                       'TestOnly\nTestOnly\nok\texample.test/fixture\t0.1s\n',
                       'TestOnly\n?\texample.test/fixture\t[no test files]\n',
                       'TestOnly\nok\twrong.package\t0.1s\n',
                       'TestOnly\nwarning: unexpected discovery output\nok\texample.test/fixture\t0.1s\n'):
            with self.subTest(output=output), self.assertRaises(ValueError):
                planner.parse_discovery(output, 'example.test/fixture')

    def test_round_robin_is_deterministic_balanced_and_exhaustive(self):
        names = ['TestCase%03d' % index for index in range(359)] + ['Example']
        shards = planner.partition(names, 4)
        self.assertEqual(shards, planner.partition(list(reversed(names)), 4))
        self.assertEqual([len(shard) for shard in shards], [90, 90, 90, 90])
        self.assertEqual(collections.Counter(name for shard in shards for name in shard),
                         collections.Counter(names))
        self.assertEqual(sum('Example' in shard for shard in shards), 1)

    def test_refuses_empty_shards_and_duplicate_names(self):
        for names, count in (([], 4), (['TestOnly'], 0), (['TestOnly'], 2),
                             (['TestDuplicate', 'TestDuplicate'], 1)):
            with self.subTest(names=names, count=count), self.assertRaises(ValueError):
                planner.partition(names, count)
        with self.assertRaises(ValueError):
            planner.run_pattern([])

    def test_package_discovery_refuses_empty_duplicate_or_invalid_paths(self):
        for output in ('', 'example/pkg\nexample/pkg\n', 'example/pkg\n\n', 'bad path\n'):
            with self.subTest(output=output), self.assertRaises(ValueError):
                planner.parse_packages(output)

    def test_other_packages_excludes_only_registry_and_retains_native_smoke(self):
        packages = ['example.test/internal/registry', 'example.test/cmd/gateway',
                    'example.test/native/piper/smoke', 'example.test/migrations']
        with tempfile.TemporaryDirectory() as directory:
            manifest = pathlib.Path(directory) / 'plan.json'
            with mock.patch.object(planner, 'go_output', side_effect=[
                    '\n'.join(packages) + '\n', packages[0] + '\n']):
                selected = planner.other_packages(manifest).splitlines()
            self.assertEqual(selected, sorted(packages[1:]))
            self.assertEqual(json.loads(manifest.read_text())['packages'], selected)
            with mock.patch.object(planner, 'go_output', side_effect=[
                    '\n'.join(packages[1:]) + '\n', packages[0] + '\n']):
                with self.assertRaisesRegex(ValueError, 'missing the registry'):
                    planner.other_packages(manifest)


class ResultAuditTests(unittest.TestCase):
    manifest = {'packages': ['example.test/fixture'], 'selected_names': ['TestSelected']}

    def complete_events(self):
        return [event('start'), event('run', 'TestSelected'),
                event('run', 'TestSelected/child'), event('pass', 'TestSelected/child'),
                event('pass', 'TestSelected'), event('pass')]

    def test_accepts_every_subtest_without_treating_it_as_an_extra_top_level_test(self):
        self.assertEqual(planner.audit_results(self.complete_events(), self.manifest)['errors'], [])

    def test_rejects_missing_unexpected_duplicate_skipped_and_unfinished_registry_tests(self):
        missing = [event('start'), event('pass')]
        unexpected = self.complete_events() + [event('run', 'TestUnselected'), event('pass', 'TestUnselected')]
        duplicate = self.complete_events() + [event('run', 'TestSelected'), event('pass', 'TestSelected')]
        skipped = [event('start'), event('run', 'TestSelected'), event('skip', 'TestSelected'), event('pass')]
        skipped_child = self.complete_events() + [event('run', 'TestSelected/skipped'),
                                                 event('skip', 'TestSelected/skipped')]
        unfinished = [event('start'), event('run', 'TestSelected')]
        failed = [event('start'), event('run', 'TestSelected'), event('fail', 'TestSelected'), event('fail')]
        for events in (missing, unexpected, duplicate, skipped, skipped_child, unfinished, failed):
            with self.subTest(events=events):
                self.assertTrue(planner.audit_results(events, self.manifest)['errors'])

    def test_rejects_missing_packages_and_exposes_existing_native_skips(self):
        events = [event('start'), event('run', 'TestNative'), event('skip', 'TestNative'), event('pass')]
        manifest = {'packages': ['example.test/fixture']}
        result = planner.audit_results(events, manifest)
        self.assertEqual(result['errors'], [])
        self.assertEqual(result['endings'][('example.test/fixture', 'TestNative')], 'skip')
        self.assertTrue(planner.audit_results(events, {'packages': ['example.test/missing']})['errors'])
        self.assertTrue(planner.audit_results([], manifest)['errors'])


@unittest.skipUnless(shutil.which('go'), 'Go is required to verify real test discovery semantics')
class RealGoSelectionTests(unittest.TestCase):
    def test_shards_execute_all_tests_subtests_external_tests_examples_and_fuzz_seeds_once(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / 'go.mod').write_text('module example.test/fixture\n\ngo 1.20\n')
            (root / 'fixture.go').write_text('package fixture\n')
            (root / 'fixture_test.go').write_text('''package fixture
import ("fmt"; "testing")
func TestAlpha(t *testing.T) { t.Run("child", func(t *testing.T) {}) }
func TestAlphabet(t *testing.T) {}
func FuzzSeed(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, value string) {}) }
func Example() { fmt.Println("example ran")
// Output: example ran
}
func BenchmarkExcluded(b *testing.B) {}
''')
            (root / 'external_test.go').write_text('''package fixture_test
import "testing"
func TestExternal(t *testing.T) {}
''')
            env = dict(os.environ, GOWORK='off', GOFLAGS='')
            listing = subprocess.run(['go', 'test', '-count=1', '-list', planner.LIST_PATTERN, '.'],
                                     cwd=root, env=env, check=True, text=True, capture_output=True).stdout
            names = planner.parse_discovery(listing, 'example.test/fixture')
            self.assertEqual(names, ['Example', 'FuzzSeed', 'TestAlpha', 'TestAlphabet', 'TestExternal'])
            all_runs = collections.Counter()
            for shard in planner.partition(names, 4):
                completed = subprocess.run(['go', 'test', '-json', '-count=1', '-run',
                                            planner.run_pattern(shard), '.'], cwd=root, env=env,
                                           check=True, text=True, capture_output=True)
                events = [json.loads(line) for line in completed.stdout.splitlines()]
                result = planner.audit_results(events, {'packages': ['example.test/fixture'],
                                                         'selected_names': shard})
                self.assertEqual(result['errors'], [])
                all_runs.update(test for package, test in result['starts'])
            self.assertEqual(all_runs, collections.Counter(names + ['TestAlpha/child', 'FuzzSeed/seed#0']))

            (root / 'internal/registry').mkdir(parents=True)
            (root / 'internal/registry/registry.go').write_text('package registry\n')
            (root / 'raceonly').mkdir()
            (root / 'raceonly/race.go').write_text('//go:build race\n\npackage raceonly\n')

            def fixture_go_output(*args):
                return subprocess.run(['go', *args], cwd=root, env=env, check=True,
                                      text=True, capture_output=True).stdout

            with mock.patch.object(planner, 'go_output', side_effect=fixture_go_output):
                selected = planner.other_packages(root / 'packages-plan.json').splitlines()
            self.assertEqual(selected, ['example.test/fixture', 'example.test/fixture/raceonly'])


class ShellRunnerTests(unittest.TestCase):
    def run_with_fake_go(self, go_script, mode, tee_script=None):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            executable = root / 'go'
            executable.write_text('#!/usr/bin/env python3\n' + go_script)
            executable.chmod(0o755)
            if tee_script:
                tee = root / 'tee'
                tee.write_text('#!/usr/bin/env python3\n' + tee_script)
                tee.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       CI_LOG_DIR=str(root / 'logs'))
            # Keep tests from writing their fixture summaries into the parent CI step.
            env.pop('GITHUB_STEP_SUMMARY', None)
            return subprocess.run([str(ROOT / 'scripts/test-go.sh'), *mode],
                                  env=env, text=True, capture_output=True)

    def test_package_discovery_failure_stops_before_testing(self):
        result = self.run_with_fake_go('import sys\nprint("discovery failed", file=sys.stderr)\nsys.exit(23)\n',
                                       ['packages'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('discovery failed', result.stderr)
        self.assertNotIn('Race test JSON and timings:', result.stdout)

    def test_go_failure_is_not_masked_by_tee_or_successful_json_audit(self):
        result = self.run_with_fake_go('''import json, sys
print(json.dumps({"Action":"start", "Package":"example.test/fixture"}))
print(json.dumps({"Action":"pass", "Package":"example.test/fixture"}))
sys.exit(23)
''', ['all'])
        self.assertNotEqual(result.returncode, 0)

    def test_empty_successful_go_output_fails_the_runtime_audit(self):
        result = self.run_with_fake_go('pass\n', ['all'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('contains no package starts', result.stdout)

    def test_log_writer_failure_is_not_masked_by_successful_go_and_complete_json(self):
        result = self.run_with_fake_go('''import json
print(json.dumps({"Action":"start", "Package":"example.test/fixture"}))
print(json.dumps({"Action":"pass", "Package":"example.test/fixture"}))
''', ['all'], tee_script='''import pathlib, sys
value = sys.stdin.read()
pathlib.Path(sys.argv[1]).write_text(value)
sys.stdout.write(value)
sys.exit(23)
''')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('1 packages;', result.stdout)


if __name__ == '__main__':
    unittest.main()
