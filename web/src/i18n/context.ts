import { createContext } from 'react';
import en from './en';
import pl from './pl';
import de from './de';
import cs from './cs';
import es from './es';
import fr from './fr';
import pt from './pt';
import lv from './lv';
import uk from './uk';

export type TranslationKey = keyof typeof en;
export type Translations = Partial<Record<TranslationKey, string>>;
export type Locale = 'en' | 'pl' | 'de' | 'cs' | 'es' | 'fr' | 'pt' | 'lv' | 'uk';

export const locales: { code: Locale; name: string }[] = [
  { code: 'en', name: 'English' },
  { code: 'pl', name: 'Polski' },
  { code: 'de', name: 'Deutsch' },
  { code: 'cs', name: '\u010Ce\u0161tina' },
  { code: 'es', name: 'Espa\u00f1ol' },
  { code: 'fr', name: 'Fran\u00e7ais' },
  { code: 'pt', name: 'Portugu\u00eas' },
  { code: 'lv', name: 'Latvie\u0161u' },
  { code: 'uk', name: '\u0423\u043a\u0440\u0430\u0457\u043d\u0441\u044c\u043a\u0430' },
];

export const allTranslations: Record<Locale, Translations> = { en, pl, de, cs, es, fr, pt, lv, uk };

export interface I18nContextValue {
  t: (key: TranslationKey, vars?: Record<string, string | number>) => string;
  locale: Locale;
}

export const I18nContext = createContext<I18nContextValue>({
  t: (key) => en[key],
  locale: 'en',
});

export function browserLocale(): Locale {
  const lang = navigator.language?.toLowerCase() ?? '';
  const prefix = lang.split('-')[0] as Locale;
  if (prefix in allTranslations) return prefix;
  return 'en';
}
