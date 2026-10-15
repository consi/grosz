import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fromZonedInput, toZonedInput } from '../src/utils/schedule.ts';

test('editor uses configured timezone independently of browser timezone', () => {
  assert.equal(toZonedInput('2026-10-15T12:30:00Z', 'Europe/Warsaw'), '2026-10-15T14:30');
  assert.equal(fromZonedInput('2026-10-15T14:30', 'Europe/Warsaw'), '2026-10-15T12:30:00.000Z');
  assert.equal(fromZonedInput('2026-01-15T14:30', 'Europe/Warsaw'), '2026-01-15T13:30:00.000Z');
  assert.equal(fromZonedInput('2026-10-15T14:30', 'America/New_York'), '2026-10-15T18:30:00.000Z');
});

test('rejects nonexistent DST wall times instead of silently moving a window', () => {
  assert.throws(() => fromZonedInput('2026-03-29T02:30', 'Europe/Warsaw'));
});

test('DST transition and fractional timezone offsets roundtrip', () => {
  for (const [value, zone] of [
    ['2026-03-29T03:30', 'Europe/Warsaw'],
    ['2026-10-25T02:30', 'Europe/Warsaw'],
    ['2026-10-15T10:15', 'Asia/Kathmandu'],
  ]) {
    assert.equal(toZonedInput(fromZonedInput(value, zone), zone), value);
  }
});

test('editing an unchanged repeated DST hour preserves its original occurrence', () => {
  for (const original of ['2026-10-25T00:30:00.000Z', '2026-10-25T01:30:00.000Z']) {
    assert.equal(fromZonedInput('2026-10-25T02:30', 'Europe/Warsaw', original), original);
  }
});
