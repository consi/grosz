import { useState } from 'react';
import { prepareRequestOptions, encodeAuthenticationResponse } from '../utils/webauthn';
import { useTranslation } from '../i18n';

interface Props {
  onLogin: () => void;
  passkeysAvailable?: boolean;
}

export function Login({ onLogin, passkeysAvailable }: Props) {
  const { t } = useTranslation();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError('');

    try {
      const resp = await fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password }),
      });

      if (resp.ok) {
        onLogin();
      } else {
        setError(t('login.errorInvalid'));
      }
    } catch {
      setError(t('login.errorConnection'));
    } finally {
      setLoading(false);
    }
  };

  const handlePasskeyLogin = async () => {
    setLoading(true);
    setError('');
    try {
      const beginResp = await fetch('/api/webauthn/login/begin', { method: 'POST' });
      if (!beginResp.ok) throw new Error('Failed to start passkey login');
      const beginData = await beginResp.json();

      const requestOptions = prepareRequestOptions(beginData.options);
      const credential = await navigator.credentials.get({ publicKey: requestOptions });
      if (!credential) throw new Error('No credential returned');

      const completeResp = await fetch('/api/webauthn/login/complete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          challengeKey: beginData.challengeKey,
          ...encodeAuthenticationResponse(credential as PublicKeyCredential),
        }),
      });

      if (completeResp.ok) {
        onLogin();
      } else {
        setError('Passkey authentication failed');
      }
    } catch (err) {
      if (err instanceof DOMException && err.name === 'NotAllowedError') {
        setError('Passkey request cancelled');
      } else {
        setError(err instanceof Error ? err.message : 'Passkey login failed');
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="login-backdrop">
      <form className="login-card" onSubmit={handleSubmit}>
        <h1 className="login-title">grosz</h1>
        <p className="login-subtitle">{t('login.subtitle')}</p>

        <label className="login-field">
          <span>{t('login.username')}</span>
          <input
            type="text"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoFocus
            autoComplete="username"
          />
        </label>

        <label className="login-field">
          <span>{t('login.password')}</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
        </label>

        {error && <div className="login-error">{error}</div>}

        <button className="btn primary login-btn" type="submit" disabled={loading}>
          {loading ? t('login.signingIn') : t('login.signIn')}
        </button>

        {passkeysAvailable && (
          <>
            <div className="login-divider"><span>{t('common.or')}</span></div>
            <button
              className="btn login-btn passkey-btn"
              type="button"
              onClick={handlePasskeyLogin}
              disabled={loading}
            >
              {t('login.signInPasskey')}
            </button>
          </>
        )}
      </form>
    </div>
  );
}
