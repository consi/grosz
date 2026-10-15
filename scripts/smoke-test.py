#!/usr/bin/env python3
"""Smoke-test a binary with a fresh temporary DB inside an isolated network.
Linux usage (as root): unshare --net sh -c 'ip link set lo up; python3 scripts/smoke-test.py /usr/bin/grosz'
Never opens or modifies the installed service's database.
"""
import datetime as dt
import http.cookiejar
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

# Refuse to run without a dedicated network namespace.
if os.readlink('/proc/self/ns/net') == os.readlink('/proc/1/ns/net'):
    raise SystemExit('Run in a dedicated network namespace; see the module docstring.')

with tempfile.TemporaryDirectory(prefix='grosz-smoke-') as directory:
    db = Path(directory) / 'test.db'
    connection = sqlite3.connect(db)
    connection.execute('CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)')
    connection.executemany('INSERT INTO settings VALUES (?, ?)', {
        'web.port': '13000', 'ocpp.port': '18887',
        'auth.username': 'smoke-test', 'auth.password': 'fresh-test-database-only',
        'charger.max_power': '22000', 'scheduler.target_energy': '0',
        'scheduler.battery_capacity': '52', 'scheduler.target_soc': '85',
        'scheduler.current_soc': '75', 'scheduler.skip_above_soc': '70',
    }.items())
    connection.execute("CREATE TABLE tariff_rates (id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL, start_time TEXT NOT NULL, end_time TEXT NOT NULL, price REAL NOT NULL, fetched_at TEXT NOT NULL DEFAULT (datetime('now')), UNIQUE(provider,start_time))")
    anchor = dt.datetime.now(dt.timezone.utc).replace(minute=0, second=0, microsecond=0)
    connection.executemany('INSERT INTO tariff_rates (provider,start_time,end_time,price) VALUES (?,?,?,?)', [
        ('pstryk', (anchor + dt.timedelta(hours=i)).isoformat(), (anchor + dt.timedelta(hours=i+1)).isoformat(), .3)
        for i in range(48)
    ])
    connection.commit()
    connection.close()
    # Production cookies are Secure. This namespace-only HTTP client accepts
    # its own fresh test login cookie over loopback.
    policy = http.cookiejar.DefaultCookiePolicy(secure_protocols=('https', 'http'))
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar(policy=policy)))

    def request(path, method='GET', body=None, expected=200):
        payload = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request('http://127.0.0.1:13000' + path, data=payload,
                                     method=method, headers={'Content-Type': 'application/json'})
        try:
            with opener.open(req, timeout=5) as response:
                code, data = response.status, response.read()
        except urllib.error.HTTPError as error:
            code, data = error.code, error.read()
        assert code == expected, (path, code, data)
        return json.loads(data) if data and data[:1] in (b'{', b'[') else data

    def wait_schedule(predicate):
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            value = request('/api/status').get('schedule')
            if predicate(value):
                return
            time.sleep(0.2)
        raise AssertionError('Schedule did not update after debounce')

    with open(Path(directory) / 'app.log', 'w+') as log:
        process = subprocess.Popen([sys.argv[1], '--db', str(db)], stdout=log, stderr=log)
        try:
            for attempt in range(60):
                try:
                    version = request('/api/version')
                    break
                except (OSError, urllib.error.URLError):
                    time.sleep(0.2)
            else:
                raise AssertionError('Server did not start')
            request('/api/login', 'POST', {'username': 'smoke-test', 'password': 'fresh-test-database-only'})
            assert request('/api/auth/check')['authenticated']
            assert not request('/api/status').get('schedule'), 'automatic charging must skip above the SoC limit'
            stream = opener.open('http://127.0.0.1:13000/api/events/stream', timeout=15)
            snapshots = []
            def receive_events():
                try:
                    for line in stream:
                        if line.startswith(b'data: {'):
                            snapshots.append(json.loads(line[6:]))
                except (OSError, ValueError):
                    pass
            threading.Thread(target=receive_events, daemon=True).start()
            def wait_event(predicate):
                deadline = time.monotonic() + 12
                while time.monotonic() < deadline:
                    if any(predicate(snapshot) for snapshot in snapshots): return
                    time.sleep(.1)
                raise AssertionError('UI status event did not arrive above the SoC skip threshold')
            wait_event(lambda snapshot: snapshot.get('overrides') == [])
            assert b'<html' in request('/')
            now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
            stamp = lambda hours: (now + dt.timedelta(hours=hours)).isoformat()
            window = {'kind': 'force', 'start': stamp(1), 'end': stamp(2)}
            created = request('/api/schedule/overrides', 'POST', window)['override']
            assert created['powerW'] == 22000
            route = '/api/schedule/overrides/' + str(created['id'])
            wait_event(lambda snapshot: snapshot.get('schedule', {}).get('energy') == 22)
            wait_schedule(lambda schedule: schedule and schedule['energy'] == 22)
            repeated = request('/api/schedule/overrides', 'POST', window)['override']
            route = '/api/schedule/overrides/' + str(repeated['id'])
            created = repeated
            updated = request(route, 'PUT', {**window, 'end': stamp(3)})['override']
            assert updated['id'] == created['id'] and updated['powerW'] == 22000
            wait_event(lambda snapshot: snapshot.get('schedule', {}).get('energy') == 44)
            wait_schedule(lambda schedule: schedule and schedule['energy'] == 44)
            request(route, 'PUT', {**window, 'end': stamp(0)}, expected=422)
            saved = request('/api/schedule/overrides')['overrides']
            assert len(saved) == 1 and saved[0]['end'] == updated['end']
            request(route, 'PUT', {**window, 'kind': 'block'})
            wait_schedule(lambda schedule: not schedule)
            assert request('/api/schedule/overrides')['overrides'][0]['kind'] == 'block'
            request(route, 'DELETE')
            assert request('/api/schedule/overrides')['overrides'] == []
            request(route, 'DELETE', expected=404)
            request(route, 'PUT', window, expected=404)
            # Reproduce the reported trap: a block must be replaceable from the graph.
            request('/api/schedule/overrides', 'POST', {**window, 'kind': 'block', 'end': stamp(3)})
            restored = request('/api/schedule/overrides', 'POST', window)['override']
            assert restored['kind'] == 'force'
            wait_schedule(lambda schedule: schedule and schedule['energy'] == 22)
            request('/api/schedule', 'DELETE')
            cleared = request('/api/status')
            assert cleared.get('schedule') is None and cleared['overrides'] == []
            wait_event(lambda snapshot: snapshot.get('skipReasonKey') == 'user_cancelled')
            assert request('/api/status').get('schedule') is None
            request('/api/schedule/overrides', 'POST', window)
            wait_schedule(lambda schedule: schedule and schedule['energy'] == 22)
            request('/api/schedule', 'DELETE')
            request('/api/schedule', 'POST')
            assert request('/api/settings')['scheduler.paused'] == 'false'
            connection = sqlite3.connect(db)
            assert connection.execute('PRAGMA quick_check').fetchone()[0] == 'ok'
            connection.close()
            print(json.dumps({'result': 'PASS', 'version': version,
                              'checks': ['SSE snapshot and updates above SoC skip threshold', 'embedded UI', 'login', 'maximum power', 'create', 'debounce',
                                         'atomic edit', 'overlap replacement', 'invalid edit preservation',
                                         'block', 'delete', 'restore blocked range', 'clear everything', 'manual while paused', 'resume automatic', '404', 'SQLite integrity']}))
        except Exception:
            log.flush()
            log.seek(0)
            print(log.read(), file=sys.stderr)
            raise
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
