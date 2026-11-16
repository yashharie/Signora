'use client';
import Link from 'next/link';
import { Upload, PenTool, Users, Activity } from 'lucide-react';

interface WorkflowStepperProps {
  docId: string;
  currentStep: 'prepare' | 'signers' | 'status';
}

export default function WorkflowStepper({ docId, currentStep }: WorkflowStepperProps) {
  const steps = [
    { key: 'signers', label: '1. Add Signers', icon: Users, href: `/documents/${docId}/signers` },
    { key: 'prepare', label: '2. Place Boxes on PDF', icon: PenTool, href: `/documents/${docId}/prepare` },
    { key: 'status', label: '3. Status', icon: Activity, href: `/documents/${docId}/status` },
  ];

  return (
    <nav className="stepper" aria-label="Document workflow steps">
      {steps.map((step, idx) => {
        const Icon = step.icon;
        const isActive = currentStep === step.key;
        return (
          <Link
            key={step.key}
            href={step.href}
            className={`step-item ${isActive ? 'active' : ''}`}
          >
            <div className="step-num">{idx + 1}</div>
            <Icon size={16} />
            <span>{step.label}</span>
          </Link>
        );
      })}
    </nav>
  );
}
