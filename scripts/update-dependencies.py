#!/usr/bin/env python3
"""Select stable npm and Go releases at least five days old.
Run from the repository root; installs/tidy and validation are separate steps.
"""
import concurrent.futures
import datetime as dt
import json
import re
import subprocess
import urllib.request
from pathlib import Path

CUTOFF = dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=5)

def fetch(url):
    with urllib.request.urlopen(url, timeout=60) as response:
        return json.load(response)

def version_key(version):
    return tuple(map(int, version.lstrip('v').split('.')))

def eligible(timestamp):
    return dt.datetime.fromisoformat(timestamp.replace('Z', '+00:00')) <= CUTOFF

def npm_version(name):
    metadata = fetch('https://registry.npmjs.org/' + name.replace('/', '%2f'))
    # TypeScript ESLint supports TS <6.1; Babel consumers still require v7.
    compatible = {'typescript': (6, 0), '@babel/core': (7,)}.get(name, ())
    versions = [v for v, stamp in metadata['time'].items()
                if re.fullmatch(r'\d+\.\d+\.\d+', v) and eligible(stamp)
                and version_key(v)[:len(compatible)] == compatible
                and not metadata['versions'].get(v, {}).get('deprecated')]
    return name, max(versions, key=version_key)

def go_version(module):
    path, current = module['Path'], module['Version']
    escaped = ''.join('!' + c.lower() if c.isupper() else c for c in path)
    base = 'https://proxy.golang.org/' + escaped
    with urllib.request.urlopen(base + '/@v/list', timeout=60) as response:
        versions = response.read().decode().splitlines()
    for version in sorted((v for v in versions if re.fullmatch(r'v\d+\.\d+\.\d+', v)), key=version_key, reverse=True):
        if version_key(version) < version_key(current.split('+')[0].split('-')[0]):
            break
        if eligible(fetch(base + '/@v/' + version + '.info')['Time']):
            return path, current, version
    return path, current, current

if __name__ == '__main__':
    print('Release cutoff:', CUTOFF.isoformat(), flush=True)
    package_path = Path('web/package.json')
    package = json.loads(package_path.read_text())
    names = set(package['dependencies']) | set(package['devDependencies'])
    names |= {k for k, v in package.get('overrides', {}).items() if isinstance(v, str)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        latest = dict(pool.map(npm_version, sorted(names)))
    for section in ('dependencies', 'devDependencies', 'overrides'):
        for name, current in list(package[section].items()):
            if isinstance(current, str):
                print('npm', name, current, '->', latest[name], flush=True)
                package[section][name] = latest[name]
    # Keep the scoped eslint override attached to the selected plugin version.
    overrides = package['overrides']
    for key in list(overrides):
        if key.startswith('@typescript-eslint/eslint-plugin@'):
            value = overrides.pop(key)
            overrides['@typescript-eslint/eslint-plugin@' + latest['typescript-eslint']] = value
    package_path.write_text(json.dumps(package, indent=2) + '\n')
    # Update declared runtime dependencies only. Test dependencies of dependencies
    # must not be promoted into this project's module or force downgrades.
    parsed = [{'Path': path, 'Version': version} for path, version in
              re.findall(r'^\s+([^\s]+) (v[^\s]+)', Path('go.mod').read_text(), re.MULTILINE)]
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        updates = list(pool.map(go_version, parsed))
    changes = []
    for path, current, latest_version in updates:
        print('go', path, current, '->', latest_version, flush=True)
        if current != latest_version:
            changes.append(path + '@' + latest_version)
    if changes:
        subprocess.run(['go', 'get', *changes], check=True)
    print('In web, run: npm update --before=' + CUTOFF.isoformat(), flush=True)
