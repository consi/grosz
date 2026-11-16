import { test } from 'node:test';
import assert from 'node:assert/strict';
import { caretContext, childFields, jsonError, tokenize, validatePayload, type ConsoleField } from '../src/utils/ocppConsole.ts';

const fields: ConsoleField[] = [
  { path: 'connectorId', type: 'integer', required: true },
  { path: 'csChargingProfiles', type: 'object', required: true },
  { path: 'csChargingProfiles.chargingProfilePurpose', type: 'string', required: true, enum: ['TxDefaultProfile', 'TxProfile'] },
  { path: 'csChargingProfiles.chargingSchedule', type: 'object', required: true },
  { path: 'csChargingProfiles.chargingSchedule.chargingSchedulePeriod', type: 'array', required: true },
  { path: 'csChargingProfiles.chargingSchedule.chargingSchedulePeriod[].limit', type: 'number', required: true },
  { path: 'csChargingProfiles.chargingSchedule.chargingSchedulePeriod[].startPeriod', type: 'integer', required: true },
];

test('tokenizer reproduces input and classifies keys and values', () => {
  const text = '{"a": 1, "b": [true, null, "x"], bad}';
  const tokens = tokenize(text);
  assert.equal(tokens.map((t) => t.text).join(''), text);
  assert.deepEqual(tokens.filter((t) => t.type !== 'space' && t.type !== 'punct').map((t) => t.type),
    ['key', 'number', 'key', 'boolean', 'null', 'string', 'plain']);
  assert.equal(tokenize('{"unterminated').map((t) => t.text).join(''), '{"unterminated');
});

test('json errors report line and column', () => {
  assert.equal(jsonError('{"a": 1}'), null);
  const err = jsonError('{\n  "a": 1,\n}');
  assert.ok(err);
  assert.equal(err.line, 3);
});

test('schema warnings: unknown, missing, enum, type, nested arrays', () => {
  const warnings = validatePayload({
    connectorId: 1.5, extra: 1,
    csChargingProfiles: { chargingProfilePurpose: 'Nope', chargingSchedule: { chargingSchedulePeriod: [{ limit: 1, foo: 2 }] } },
  }, fields);
  const summary = warnings.map((w) => `${w.kind}:${w.path}`).sort();
  assert.deepEqual(summary, [
    'enum:csChargingProfiles.chargingProfilePurpose',
    'missing:csChargingProfiles.chargingSchedule.chargingSchedulePeriod[].startPeriod',
    'type:connectorId',
    'unknown:csChargingProfiles.chargingSchedule.chargingSchedulePeriod[].foo',
    'unknown:extra',
  ]);
});

test('caret context finds key and value positions with paths', () => {
  const text = '{"connectorId": 1, "csChargingProfiles": {"chargingSchedule": {"chargingSchedulePeriod": [{"li';
  const ctx = caretContext(text, text.length);
  assert.equal(ctx?.mode, 'key');
  assert.equal(ctx?.path, 'csChargingProfiles.chargingSchedule.chargingSchedulePeriod[]');
  assert.equal(ctx?.partial, 'li');
  assert.deepEqual(childFields(fields, ctx!.path).map((f) => f.path.split('.').pop()), ['limit', 'startPeriod']);

  const value = '{"csChargingProfiles": {"chargingProfilePurpose": "Tx';
  const v = caretContext(value, value.length);
  assert.equal(v?.mode, 'value');
  assert.equal(v?.key, 'chargingProfilePurpose');
  assert.equal(v?.path, 'csChargingProfiles');
  assert.equal(v?.partial, 'Tx');

  const root = '{"connectorId": 1, ';
  const r = caretContext(root, root.length);
  assert.equal(r?.mode, 'key');
  assert.deepEqual(r?.existing, ['connectorId']);
  assert.equal(caretContext('{"connectorId": 1', 17)?.mode, 'value');
  assert.equal(caretContext('{"type": "Soft" ', 16), null);
});
