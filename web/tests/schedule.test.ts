import { test } from 'node:test';
import assert from 'node:assert/strict';
import { covers, fromZonedInput, nextDeparture, overlaps, toZonedInput } from '../src/utils/schedule.ts';

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

test('next departure is the next future occurrence in the display timezone', () => {
  // 20:00 Warsaw (CEST) on 15 Oct: departure is tomorrow 07:00 local.
  assert.equal(nextDeparture('07:00', 'Europe/Warsaw', new Date('2026-10-15T18:00:00Z')), '2026-10-16T05:00:00.000Z');
  // 05:00 Warsaw: departure is later the same day.
  assert.equal(nextDeparture('07:00', 'Europe/Warsaw', new Date('2026-10-15T03:00:00Z')), '2026-10-15T05:00:00.000Z');
  // Exactly at departure: the next one is a day later (winter time after the DST change).
  assert.equal(nextDeparture('7:00', 'Europe/Warsaw', new Date('2026-10-25T06:00:00Z')), '2026-10-26T06:00:00.000Z');
});

test('override overlap helpers', () => {
  const hour = { start: '2026-10-15T10:00:00Z', end: '2026-10-15T11:00:00Z' };
  assert.ok(overlaps(hour, { start: '2026-10-15T10:30:00Z', end: '2026-10-15T12:00:00Z' }));
  assert.ok(!overlaps(hour, { start: '2026-10-15T11:00:00Z', end: '2026-10-15T12:00:00Z' }));
  assert.ok(covers({ start: '2026-10-15T09:00:00Z', end: '2026-10-15T12:00:00Z' }, hour));
  assert.ok(!covers({ start: '2026-10-15T10:30:00Z', end: '2026-10-15T12:00:00Z' }, hour));
});
