import { useTranslation } from '../i18n';

export function VersionUpdateBanner() {
  const { t } = useTranslation();
  return (
    <div className="version-banner" role="status" aria-live="polite">
      <span>{t('version.newVersion')}</span>
      <button className="btn primary btn-sm" onClick={() => location.reload()}>
        {t('version.reload')}
      </button>
    </div>
  );
}
