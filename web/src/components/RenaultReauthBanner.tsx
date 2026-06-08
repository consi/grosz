import { useTranslation } from '../i18n';

export function RenaultReauthBanner({ onFix }: { onFix: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="version-banner" role="status" aria-live="polite">
      <span>{t('renault.reauthBanner')}</span>
      <button className="btn primary btn-sm" onClick={onFix}>
        {t('renault.reauthFix')}
      </button>
    </div>
  );
}
