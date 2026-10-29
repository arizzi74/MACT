#!/usr/bin/env node
// Audit the build lockfile AND every version actually compiled into the browser
// bundle, including packages Mermaid ships prebundled outside our npm tree.
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const inventory = JSON.parse(await readFile(path.join(root, 'internal/admin/static/webui-mermaid-dependencies.json'), 'utf8'));
if (inventory.schema !== 1 || !Array.isArray(inventory.packages) || !inventory.packages.length) throw new Error('Invalid browser dependency inventory.');
const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
function audit(directory, label) {
  const result = spawnSync(npm, ['audit', '--package-lock-only', '--ignore-scripts', '--audit-level=low', '--json'], { cwd: directory, encoding: 'utf8', timeout: 120000 });
  if (result.error) throw result.error;
  let report;
  try { report = JSON.parse(result.stdout); }
  catch { throw new Error(`${label}: npm audit did not return a report (exit ${result.status}).`); }
  if (report.error || !report.metadata?.vulnerabilities || ![0, 1].includes(result.status)) throw new Error(`${label}: advisory service or npm audit failed; scan is incomplete.`);
  const total = report.metadata.vulnerabilities.total;
  console.log(`${label}: ${total} advisory finding(s).`);
  for (const finding of Object.values(report.vulnerabilities ?? {})) {
    for (const advisory of finding.via.filter(value => typeof value === 'object')) console.log(`${finding.name}: ${advisory.severity}; ${advisory.title}; ${advisory.url}`);
  }
  return total === 0 && result.status === 0;
}

const directory = await mkdtemp(path.join(tmpdir(), 'mact-browser-audit-'));
try {
  const manifest = { name: 'mact-shipped-browser-inventory', version: '0.0.0', private: true, dependencies: {} };
  const packages = { '': manifest };
  inventory.packages.forEach(({ name, version }, index) => {
    if (!/^(@[a-z0-9_.-]+\/)?[a-z0-9_.-]+$/.test(name) || !/^\d+\.\d+\.\d+(?:[-+][a-zA-Z0-9.-]+)?$/.test(version)) throw new Error('Invalid browser package identity.');
    // Aliases preserve multiple shipped versions of a package in npm's virtual
    // tree. The package name, rather than the alias, is sent for advisory lookup.
    const alias = `bundled-${index}`;
    manifest.dependencies[alias] = `npm:${name}@${version}`;
    packages[`node_modules/${alias}`] = { name, version };
  });
  await writeFile(path.join(directory, 'package.json'), JSON.stringify(manifest));
  await writeFile(path.join(directory, 'package-lock.json'), JSON.stringify({ name: manifest.name, version: manifest.version, lockfileVersion: 3, requires: true, packages }));
  const lockClean = audit(root, 'Build lockfile (including development dependencies)');
  const bundleClean = audit(directory, `Shipped browser bundle (${inventory.packages.length} exact package versions)`);
  if (!lockClean || !bundleClean) process.exitCode = 1;
} finally {
  await rm(directory, { recursive: true, force: true });
}
