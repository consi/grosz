import { useState, useEffect } from 'react';
import { prepareCreationOptions, encodeRegistrationResponse } from '../utils/webauthn';
import { useTranslation } from '../i18n';

interface Credential {
  id: string;
  name: string;
  createdAt: string;
}

export function PasskeyManager() {
  const { t, locale } = useTranslation();
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [showRegister, setShowRegister] = useState(false);
  const [newName, setNewName] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  const fetchCredentials = () => {
    fetch('/api/webauthn/credentials')
      .then((r) => r.json())
      .then((d) => setCredentials(d.credentials || []))
      .catch(() => {});
  };

  useEffect(fetchCredentials, []);

  const handleRegister = async () => {
    if (!newName.trim()) return;
    setLoading(true);
    setError('');
    try {
      const beginResp = await fetch('/api/webauthn/register/begin', { method: 'POST' });
      if (!beginResp.ok) throw new Error('Failed to start registration');
      const beginData = await beginResp.json();

      const creationOptions = prepareCreationOptions(beginData.options);
      const credential = await navigator.credentials.create({ publicKey: creationOptions });
      if (!credential) throw new Error('No credential returned');

      const completeResp = await fetch('/api/webauthn/register/complete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          challengeKey: beginData.challengeKey,
          name: newName.trim(),
          ...encodeRegistrationResponse(credential as PublicKeyCredential),
        }),
      });

      if (!completeResp.ok) {
        const data = await completeResp.text();
        throw new Error(data || t('passkey.failed'));
      }

      setNewName('');
      setShowRegister(false);
      fetchCredentials();
    } catch (err) {
      if (err instanceof DOMException && err.name === 'NotAllowedError') {
        setError(t('passkey.cancelled'));
      } else {
        setError(err instanceof Error ? err.message : t('passkey.failed'));
      }
    } finally {
      setLoading(false);
    }
  };

  const handleDelete = async (id: string, name: string) => {
    if (!confirm(t('passkey.deleteConfirm', { name }))) return;
    await fetch(`/api/webauthn/credentials/${encodeURIComponent(id)}`, { method: 'DELETE' });
    fetchCredentials();
  };

  return (
    <div>
      {credentials.length > 0 && (
        <div className="passkey-list">
          {credentials.map((c) => (
            <div key={c.id} className="passkey-row">
              <div>
                <span className="passkey-name">{c.name}</span>
                <span className="passkey-date">{new Date(c.createdAt).toLocaleDateString(locale)}</span>
              </div>
              <button className="btn btn-sm" onClick={() => handleDelete(c.id, c.name)}>{t('common.delete')}</button>
            </div>
          ))}
        </div>
      )}

      {error && <div className="login-error" style={{ marginBottom: '0.5rem' }}>{error}</div>}

      {showRegister ? (
        <div className="passkey-register">
          <input
            type="text"
            placeholder={t('passkey.name')}
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && handleRegister()}
            autoFocus
          />
          <button className="btn primary btn-sm" onClick={handleRegister} disabled={loading || !newName.trim()}>
            {loading ? t('passkey.registering') : t('passkey.register')}
          </button>
          <button className="btn btn-sm" onClick={() => { setShowRegister(false); setError(''); }}>{t('common.cancel')}</button>
        </div>
      ) : (
        <button className="btn primary btn-sm" onClick={() => setShowRegister(true)}>{t('passkey.addPasskey')}</button>
      )}

      <div className="passkey-note">
        {t('passkey.hostNote', { hostname: window.location.hostname })}
      </div>
    </div>
  );
}
