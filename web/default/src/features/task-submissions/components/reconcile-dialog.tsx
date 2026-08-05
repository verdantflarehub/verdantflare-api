/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/

import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'

import type { ReconcilePayload, TaskSubmissionDetail } from '../types'

type ReconcileDialogProps = {
  submission: TaskSubmissionDetail | null
  open: boolean
  pending: boolean
  onOpenChange: (open: boolean) => void
  onSubmit: (payload: ReconcilePayload) => void
}

export function ReconcileDialog(props: ReconcileDialogProps) {
  const { t } = useTranslation()
  const [decision, setDecision] = useState<'not_created' | 'bind'>(
    'not_created'
  )
  const [evidence, setEvidence] = useState('')
  const [reason, setReason] = useState('')
  const [upstreamTaskId, setUpstreamTaskId] = useState('')
  const [providerRequestId, setProviderRequestId] = useState('')

  function submit() {
    if (!props.submission || !evidence.trim() || !reason.trim()) return
    props.onSubmit({
      expected_version: props.submission.version,
      decision,
      evidence: evidence.trim(),
      reason: reason.trim(),
      upstream_task_id: decision === 'bind' ? upstreamTaskId.trim() : undefined,
      provider_request_id:
        decision === 'bind' ? providerRequestId.trim() : undefined,
    })
  }

  const valid =
    Boolean(evidence.trim()) &&
    Boolean(reason.trim()) &&
    (decision !== 'bind' || Boolean(upstreamTaskId.trim()))

  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className='sm:max-w-lg'>
        <DialogHeader>
          <DialogTitle>{t('Reconcile UNKNOWN submission')}</DialogTitle>
          <DialogDescription>
            {t(
              'Binding requires exact provider-request correlation or approvals from two distinct signed-in administrators.'
            )}
          </DialogDescription>
        </DialogHeader>
        <div className='space-y-3'>
          <label className='block space-y-1 text-sm'>
            <span>{t('Decision')}</span>
            <NativeSelect
              className='w-full'
              value={decision}
              onChange={(event) =>
                setDecision(event.target.value as 'not_created' | 'bind')
              }
            >
              <NativeSelectOption value='not_created'>
                {t('Provider task was not created — refund')}
              </NativeSelectOption>
              <NativeSelectOption value='bind'>
                {t('Bind confirmed provider task')}
              </NativeSelectOption>
            </NativeSelect>
          </label>
          {decision === 'bind' && (
            <>
              <Input
                aria-label={t('Upstream task ID')}
                placeholder={t('Upstream task ID (required)')}
                value={upstreamTaskId}
                onChange={(event) => setUpstreamTaskId(event.target.value)}
              />
              <Input
                aria-label={t('Provider request ID')}
                placeholder={t(
                  'Provider request ID (optional exact correlation)'
                )}
                value={providerRequestId}
                onChange={(event) => setProviderRequestId(event.target.value)}
              />
            </>
          )}
          <Textarea
            aria-label={t('Evidence')}
            placeholder={t(
              'Evidence (required; stored in the admin audit trail)'
            )}
            value={evidence}
            onChange={(event) => setEvidence(event.target.value)}
          />
          <Textarea
            aria-label={t('Reason')}
            placeholder={t('Reason (required)')}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </div>
        <DialogFooter>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            disabled={!valid || props.pending}
            onClick={submit}
          >
            {props.pending ? t('Submitting...') : t('Submit review')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
