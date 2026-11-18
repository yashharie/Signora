'use client';
import Link from 'next/link';
import { PenTool, Users, Activity } from 'lucide-react';

interface WorkflowStepperProps {
  docId: string;
  currentStep: 'prepare' | 'signers' | 'status';
}

export default function WorkflowStepper({ docId, currentStep }: WorkflowStepperProps) {
  const steps = [
    { key: 'signers', label: 'Manage Signers', icon: Users, href: `/documents/${docId}/signers` },
    { key: 'prepare', label: 'Prepare & Send', icon: PenTool, href: `/documents/${docId}/prepare` },
    { key: 'status', label: 'Status', icon: Activity, href: `/documents/${docId}/status` },
  ];

  return (
    <nav className="stepper" aria-label="Document workflow steps">
      {steps.map((step) => {
        const Icon = step.icon;
        const isActive = currentStep === step.key;
        return (
          <Link
            key={step.key}
            href={step.href}
            aria-current={isActive ? 'page' : undefined}
            className={`step-item ${isActive ? 'active' : ''}`}
          >
            <Icon size={16} />
            <span>{step.label}</span>
          </Link>
        );
      })}
    </nav>
  );
}
