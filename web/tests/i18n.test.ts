import { test } from 'node:test';
import assert from 'node:assert/strict';
import en from '../src/i18n/en.ts';
import cs from '../src/i18n/cs.ts';
import de from '../src/i18n/de.ts';
import es from '../src/i18n/es.ts';
import fr from '../src/i18n/fr.ts';
import lv from '../src/i18n/lv.ts';
import pl from '../src/i18n/pl.ts';
import pt from '../src/i18n/pt.ts';
import uk from '../src/i18n/uk.ts';

const languages: Record<string, Record<string, string | undefined>> = { cs, de, es, fr, lv, pl, pt, uk };
const placeholders = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();

test('every language has exactly the English key set', () => {
  const keys = Object.keys(en).sort();
  for (const [lang, strings] of Object.entries(languages)) {
    assert.deepEqual(Object.keys(strings).sort(), keys, `${lang} key set differs from en`);
  }
});

test('translations keep the English placeholders', () => {
  for (const [lang, strings] of Object.entries(languages)) {
    for (const [key, value] of Object.entries(en)) {
      assert.deepEqual(placeholders(strings[key] ?? ''), placeholders(value), `${lang}: ${key}`);
    }
  }
});

test('no em-dashes in UI strings', () => {
  for (const [lang, strings] of Object.entries({ en, ...languages })) {
    for (const [key, value] of Object.entries(strings)) {
      assert.ok(!value?.includes(String.fromCharCode(0x2014)), `${lang}: ${key}`);
    }
  }
});
