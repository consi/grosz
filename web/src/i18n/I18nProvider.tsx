import { useMemo, type ReactNode } from 'react';
import en from './en';
import { allTranslations, I18nContext, type I18nContextValue, type Locale, type TranslationKey } from './context';

export function I18nProvider({ locale, children }: { locale: Locale; children: ReactNode }) {
  const value = useMemo<I18nContextValue>(() => {
    const strings = allTranslations[locale] ?? en;
    return {
      locale,
      t: (key: TranslationKey, vars?: Record<string, string | number>) => {
        const template = strings[key] ?? en[key];
        if (!vars) return template;
        return template.replace(/\{(\w+)\}/g, (_, key) => {
          const replacement = vars[key];
          return replacement != null ? String(replacement) : `{${key}}`;
        });
      },
    };
  }, [locale]);

  return <I18nContext value={value}>{children}</I18nContext>;
}
