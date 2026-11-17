'use client';
import { FormEvent, useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { api, getError } from '../../lib/api';

export default function ResetPasswordPage() {
  const initialized = useRef(false);
  const [token, setToken] = useState('');
  const [ready, setReady] = useState(false);
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  useEffect(() => {
    if (initialized.current) return;
    initialized.current = true;
    setToken(new URLSearchParams(window.location.hash.slice(1)).get('token') || '');
    window.history.replaceState(null, '', window.location.pathname);
    setReady(true);
  }, []);
  async function submit(event: FormEvent) {
    event.preventDefault(); setError('');
    if (password !== confirmation) { setError('Passwords do not match.'); return; }
    if (new TextEncoder().encode(password).length > 72) { setError('Password must be at most 72 bytes.'); return; }
    setLoading(true);
    try {
      const response = await api.post('/api/auth/reset-password', { token, password });
      localStorage.removeItem('token');
      setMessage(response.data.message); setPassword(''); setConfirmation('');
    } catch (err) { setError(getError(err)); }
    finally { setLoading(false); }
  }
  return <main className="shell"><form className="card auth" onSubmit={submit}>
    <div className="brand">Signora</div><h1>Reset password</h1>
    {error && <p className="error" role="alert">{error}</p>}
    {message ? <p role="status">{message}</p> : !ready ? <p role="status">Loading...</p> : !/^[a-f0-9]{64}$/.test(token) ? <p role="alert">Invalid reset link. <Link href="/forgot-password">Request a new link</Link>.</p> : <>
      <p className="muted">Choose a new password with at least 8 characters.</p>
      <div className="field"><label htmlFor="password">New password</label><input id="password" autoComplete="new-password" type="password" required minLength={8} value={password} onChange={e => setPassword(e.target.value)} /></div>
      <div className="field"><label htmlFor="confirmation">Confirm password</label><input id="confirmation" autoComplete="new-password" type="password" required minLength={8} value={confirmation} onChange={e => setConfirmation(e.target.value)} /></div>
      <button className="btn" disabled={loading}>{loading ? 'Updating...' : 'Update password'}</button>
      <p><Link href="/forgot-password">Request a new reset link</Link></p>
    </>}
    <p><Link href="/login">Back to login</Link></p>
  </form></main>;
}
