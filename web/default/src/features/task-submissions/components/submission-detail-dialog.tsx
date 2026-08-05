/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/

import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

import type { TaskSubmissionDetail } from '../types'

type SubmissionDetailDialogProps = {
  submission: TaskSubmissionDetail | null
  open: boolean
  onOpenChange: (open: boolean) => void
  onReconcile: () => void
  canReconcile: boolean
}

export function SubmissionDetailDialog(props: SubmissionDetailDialogProps) {
  const { t } = useTranslation()
  const submission = props.submission

  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className='max-h-[85vh] overflow-y-auto sm:max-w-3xl'>
        <DialogHeader>
          <DialogTitle>{t('Submission detail')}</DialogTitle>
          <DialogDescription>
            {t(
              'Credentials, prompts, full media URLs, signed URLs, and archived result references are not returned by this API.'
            )}
          </DialogDescription>
        </DialogHeader>
        {submission && (
          <div className='space-y-4'>
            <dl className='grid gap-2 text-sm sm:grid-cols-2'>
              <Detail
                label={t('Public task ID')}
                value={submission.public_task_id}
              />
              <Detail
                label={t('Client request ID')}
                value={submission.client_request_id}
              />
              <Detail
                label={t('User / Channel')}
                value={`${submission.user_id} / ${submission.channel_id}`}
              />
              <Detail label={t('Provider')} value={submission.provider} />
              <Detail
                label={t('Upstream task ID')}
                value={submission.upstream_task_id || '—'}
              />
              <Detail
                label={t('Provider request ID')}
                value={submission.provider_request_id || '—'}
              />
            </dl>
            <div className='flex flex-wrap gap-2'>
              <Badge variant='outline'>{submission.state}</Badge>
              <Badge variant='outline'>{submission.billing_state}</Badge>
              <Badge variant='outline'>{submission.poll_state}</Badge>
              <Badge variant='outline'>{submission.archive_state}</Badge>
            </div>
            <Snapshot
              title={t('Request summary')}
              value={submission.request_summary}
            />
            <Snapshot
              title={t('Public price snapshot')}
              value={submission.public_price_snapshot}
            />
            <Snapshot
              title={t('Provider cost snapshot')}
              value={submission.provider_cost_snapshot}
            />
            <section className='space-y-2'>
              <h3 className='font-medium'>{t('Billing audit')}</h3>
              {submission.billing_entries.length === 0 ? (
                <p className='text-muted-foreground text-sm'>
                  {t('No entries')}
                </p>
              ) : (
                <div className='space-y-2'>
                  {submission.billing_entries.map((entry) => (
                    <div
                      key={entry.id}
                      className='rounded-lg border p-2 text-xs'
                    >
                      {entry.operation}: {entry.billing_state_before} →{' '}
                      {entry.billing_state_after} · {entry.reason_code || '—'} ·{' '}
                      {entry.actor || '—'}
                    </div>
                  ))}
                </div>
              )}
            </section>
            <section className='space-y-2'>
              <h3 className='font-medium'>{t('Reconciliation audit')}</h3>
              {submission.reconciliation_reviews.length === 0 ? (
                <p className='text-muted-foreground text-sm'>
                  {t('No reviews')}
                </p>
              ) : (
                <div className='space-y-2'>
                  {submission.reconciliation_reviews.map((review) => (
                    <div
                      key={review.id}
                      className='rounded-lg border p-2 text-xs'
                    >
                      {review.status} · {review.decision} · Admin #
                      {review.admin_id} · {review.reason} ·{' '}
                      {t('Evidence recorded')}
                    </div>
                  ))}
                </div>
              )}
            </section>
            {submission.state === 'UNKNOWN' && props.canReconcile && (
              <Button type='button' onClick={props.onReconcile}>
                {t('Reconcile submission')}
              </Button>
            )}
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

function Detail(props: { label: string; value: string }) {
  return (
    <div className='min-w-0 rounded-lg border p-2'>
      <dt className='text-muted-foreground text-xs'>{props.label}</dt>
      <dd className='truncate font-mono'>{props.value}</dd>
    </div>
  )
}

function Snapshot(props: { title: string; value?: string }) {
  if (!props.value) return null
  return (
    <section className='space-y-1'>
      <h3 className='font-medium'>{props.title}</h3>
      <pre className='bg-muted max-h-40 overflow-auto rounded-lg p-3 text-xs whitespace-pre-wrap'>
        {props.value}
      </pre>
    </section>
  )
}
