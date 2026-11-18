'use client';

import { useEffect, useState, type ChangeEvent, type FormEvent } from 'react';
import { useRouter } from 'next/navigation';
import { FileUp, ArrowLeft } from 'lucide-react';
import Link from 'next/link';
import Nav from '@/components/Nav';
import { api, getError } from '@/lib/api';

const maxSigners = 100;
const signerEmail = /^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$/i;

export default function UploadPage() {
  const router = useRouter();
  const [step, setStep] = useState<'signers' | 'pdf'>('signers');
  const [signers, setSigners] = useState([{ name: '', email: '' }]);
  const [count, setCount] = useState('1');
  const [title, setTitle] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!localStorage.getItem('token')) router.replace('/login');
  }, [router]);

  function changeCount(value: string) {
    setCount(value);
    const next = Number(value);
    if (!Number.isInteger(next) || next < 1 || next > maxSigners) return;
    setSigners((current) => Array.from({ length: next }, (_, index) => current[index] ?? { name: '', email: '' }));
  }

  function updateSigner(index: number, field: 'name' | 'email', value: string) {
    setSigners((current) => current.map((signer, i) => i === index ? { ...signer, [field]: value } : signer));
  }

  function continueToPDF(event: FormEvent) {
    event.preventDefault();
    setError('');
    const emails = signers.map((signer) => signer.email.trim().toLowerCase());
    if (signers.some((signer) => !signer.name.trim()) || emails.some((email) => !signerEmail.test(email))) {
      setError('Enter a name and valid email address for every signer.');
      return;
    }
    if (new Set(emails).size !== emails.length) {
      setError('Each signer must have a different email address.');
      return;
    }
    setStep('pdf');
  }

  function choosePDF(event: ChangeEvent<HTMLInputElement>) {
    const selected = event.target.files?.[0] ?? null;
    setError('');
    setFile(null);
    if (!selected) return;
    if (!selected.name.toLowerCase().endsWith('.pdf')) {
      setError('Only PDF files are accepted.');
      return;
    }
    if (selected.size > 20 * 1024 * 1024) {
      setError('File size must be 20 MB or less.');
      return;
    }
    setFile(selected);
    if (!title) setTitle(selected.name.replace(/\.pdf$/i, ''));
  }

  async function uploadPDF(event: FormEvent) {
    event.preventDefault();
    if (loading) return;
    setError('');
    if (!file || !title.trim()) {
      setError('Enter a document title and choose a PDF.');
      return;
    }
    const data = new FormData();
    data.append('title', title);
    data.append('file', file);
    data.append('signers', JSON.stringify(signers));
    setLoading(true);
    try {
      const { data: result } = await api.post('/api/documents/upload', data);
      router.push('/documents/' + result.document.id + '/prepare');
    } catch (err) {
      setError(getError(err));
      setLoading(false);
    }
  }

  return (
    <>
      <Nav />
      <main className="shell" style={{ maxWidth: 720 }}>
        <Link href="/dashboard" className="muted" style={{ display: 'inline-flex', gap: 6, alignItems: 'center', marginBottom: 20 }}>
          <ArrowLeft size={16} /> Back to dashboard
        </Link>
        <section className="card">
          <h1 className="title">{step === 'signers' ? '1. Add signers' : '2. Upload PDF'}</h1>
          {error && <p className="error" role="alert">{error}</p>}
          {step === 'signers' ? (
            <form onSubmit={continueToPDF}>
              <div className="field">
                <label htmlFor="signer-count">Number of signers</label>
                <input id="signer-count" type="number" min={1} max={maxSigners} required value={count} onChange={(event) => changeCount(event.target.value)} />
              </div>
              {signers.map((signer, index) => (
                <fieldset key={index} style={{ marginBottom: 16 }}>
                  <legend>Signer {index + 1}</legend>
                  <div className="field">
                    <label htmlFor={'name-' + index}>Name</label>
                    <input id={'name-' + index} required maxLength={120} value={signer.name}
                      onChange={(event) => updateSigner(index, 'name', event.target.value)} />
                  </div>
                  <div className="field">
                    <label htmlFor={'email-' + index}>Email</label>
                    <input id={'email-' + index} type="email" required maxLength={255} value={signer.email}
                      onChange={(event) => updateSigner(index, 'email', event.target.value)} />
                  </div>
                </fieldset>
              ))}
              <button className="btn" type="submit">Continue to PDF Upload</button>
            </form>
          ) : (
            <form onSubmit={uploadPDF}>
              <p className="muted">Upload the PDF for {signers.length} {signers.length === 1 ? 'signer' : 'signers'}. Maximum file size is 20 MB.</p>
              <div className="field">
                <label htmlFor="document-title">Document title</label>
                <input id="document-title" required value={title} onChange={(event) => setTitle(event.target.value)} />
              </div>
              <div className="field">
                <label htmlFor="pdf-file">PDF document</label>
                <input id="pdf-file" type="file" accept="application/pdf,.pdf" onChange={choosePDF} />
              </div>
              {file && <p className="muted">Selected: {file.name}</p>}
              <div className="actions">
                <button type="button" className="btn secondary" disabled={loading} onClick={() => { setError(''); setStep('signers'); }}>
                  <ArrowLeft size={16} /> Back to Signers
                </button>
                <button type="submit" className="btn" disabled={loading}>
                  <FileUp size={16} /> {loading ? 'Uploading...' : 'Upload and Place Signature Boxes'}
                </button>
              </div>
            </form>
          )}
        </section>
      </main>
    </>
  );
}
