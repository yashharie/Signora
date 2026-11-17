'use client';
import { FormEvent, useState } from 'react';
import Link from 'next/link';
import { api, getError } from '../../lib/api';

export default function ForgotPasswordPage() {
  const [email, setEmail] = useState('');
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault();
    setError(''); setLoading(true);
    try {
      const response = await api.post('/api/auth/forgot-password', { email });
      setMessage(response.data.message);
    } catch (err) { setError(getError(err)); }
    finally { setLoading(false); }
  }
  return <main className="shell"><form className="card auth" onSubmit={submit}>
    <div className="brand">Signora</div><h1>Forgot password?</h1>
    <p className="muted">Enter your email to request a password reset link.</p>
    {error && <p className="error" role="alert">{error}</p>}
    {message ? <p role="status">{message} Check your inbox and spam folder.</p> : <>
      <div className="field"><label htmlFor="email">Email</label><input id="email" type="email" autoComplete="email" required maxLength={255} value={email} onChange={e => setEmail(e.target.value)} /></div>
      <button className="btn" disabled={loading}>{loading ? 'Sending...' : 'Send reset link'}</button>
    </>}
    <p><Link href="/login">Back to login</Link></p>
  </form></main>;
}
