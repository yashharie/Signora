const invitationLabels = {
  sent: 'Sent',
  failed: 'Failed',
  already_sent: 'Already Sent',
  unconfirmed: 'Unconfirmed',
  sent_without_pdf: 'Sent Without PDF Attachment',
};

export interface InvitationResult {
  name: string;
  email: string;
  outcome?: keyof typeof invitationLabels;
  success: boolean;
  alreadySent?: boolean;
  error?: string;
  attachmentError?: string;
}

function invitationLabel(result: InvitationResult) {
  if (result.outcome) return invitationLabels[result.outcome];
  if (result.alreadySent) return invitationLabels.already_sent;
  if (result.success) return result.attachmentError ? invitationLabels.sent_without_pdf : invitationLabels.sent;
  return result.error?.includes('could not be confirmed') ? invitationLabels.unconfirmed : invitationLabels.failed;
}

export default function InvitationResults({ results }: { results: InvitationResult[] }) {
  return (
    <div role="status">
      {results.length > 0 && (
        <section className="card" style={{ marginTop: 16 }}>
          <h2 style={{ fontSize: 18, marginTop: 0 }}>Email Invitation Results</h2>
          <p className="muted">Sent means the mail provider accepted the email.</p>
          <ul style={{ paddingLeft: 20 }}>
            {results.map((result) => (
              <li key={result.email} style={{ marginBottom: 8 }}>
                <strong>{result.name}</strong> ({result.email}) · {invitationLabel(result)}
                {result.error && <div className="muted">{result.error}</div>}
                {result.attachmentError && <div className="muted">{result.attachmentError}</div>}
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}
