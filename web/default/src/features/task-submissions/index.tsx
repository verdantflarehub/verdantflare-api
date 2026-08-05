/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import {
  getTaskSubmission,
  getTaskSubmissions,
  reconcileTaskSubmission,
} from './api'
import { ReconcileDialog } from './components/reconcile-dialog'
import { SubmissionDetailDialog } from './components/submission-detail-dialog'
import type { ReconcilePayload, TaskSubmissionFilters } from './types'

const EMPTY_FILTERS: TaskSubmissionFilters = {
  client_request_id: '',
  public_task_id: '',
  provider_request_id: '',
  upstream_task_id: '',
  user_id: '',
  provider: '',
  channel_id: '',
  state: '',
  start_timestamp: '',
  end_timestamp: '',
  error: '',
}

export function TaskSubmissions() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const isRoot = useAuthStore(
    (state) => state.auth.user?.role === ROLE.SUPER_ADMIN
  )
  const [draft, setDraft] = useState(EMPTY_FILTERS)
  const [filters, setFilters] = useState(EMPTY_FILTERS)
  const [page, setPage] = useState(1)
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [reconcileOpen, setReconcileOpen] = useState(false)
  const pageSize = 20

  const listQuery = useQuery({
    queryKey: ['task-submissions', filters, page],
    queryFn: () => getTaskSubmissions(filters, page, pageSize),
  })
  const detailQuery = useQuery({
    queryKey: ['task-submission', selectedId],
    queryFn: () => getTaskSubmission(selectedId as number),
    enabled: selectedId !== null,
  })
  const reconcileMutation = useMutation({
    mutationFn: (payload: ReconcilePayload) =>
      reconcileTaskSubmission(selectedId as number, payload),
    onSuccess: (response) => {
      if (response.data?.requires_evidence) {
        toast.info(t('Review recorded; verified provider evidence is required'))
      } else if (response.data?.pending_review) {
        toast.info(
          t('First review recorded; a second distinct admin is required')
        )
      } else {
        toast.success(t('Submission reconciled'))
        setReconcileOpen(false)
      }
      queryClient.invalidateQueries({ queryKey: ['task-submissions'] })
      queryClient.invalidateQueries({
        queryKey: ['task-submission', selectedId],
      })
    },
  })
  const items = listQuery.data?.data?.items || []
  const total = listQuery.data?.data?.total || 0
  const detail = detailQuery.data?.data || null

  function updateFilter(key: keyof TaskSubmissionFilters, value: string) {
    setDraft((current) => ({ ...current, [key]: value }))
  }

  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>
        {t('Video Submissions')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='space-y-4'>
          <div className='grid gap-2 rounded-xl border p-3 sm:grid-cols-2 lg:grid-cols-5'>
            <FilterInput
              label={t('Client request ID')}
              value={draft.client_request_id}
              onChange={(value) => updateFilter('client_request_id', value)}
            />
            <FilterInput
              label={t('Public task ID')}
              value={draft.public_task_id}
              onChange={(value) => updateFilter('public_task_id', value)}
            />
            <FilterInput
              label={t('Provider request ID')}
              value={draft.provider_request_id}
              onChange={(value) => updateFilter('provider_request_id', value)}
            />
            <FilterInput
              label={t('Upstream task ID')}
              value={draft.upstream_task_id}
              onChange={(value) => updateFilter('upstream_task_id', value)}
            />
            <FilterInput
              label={t('User ID')}
              value={draft.user_id}
              onChange={(value) => updateFilter('user_id', value)}
            />
            <FilterInput
              label={t('Provider')}
              value={draft.provider}
              onChange={(value) => updateFilter('provider', value)}
            />
            <FilterInput
              label={t('Channel ID')}
              value={draft.channel_id}
              onChange={(value) => updateFilter('channel_id', value)}
            />
            <NativeSelect
              className='w-full'
              value={draft.state}
              onChange={(event) => updateFilter('state', event.target.value)}
              aria-label={t('State')}
            >
              <NativeSelectOption value=''>
                {t('All states')}
              </NativeSelectOption>
              {['PREPARED', 'SENDING', 'CONFIRMED', 'REJECTED', 'UNKNOWN'].map(
                (state) => (
                  <NativeSelectOption key={state} value={state}>
                    {state}
                  </NativeSelectOption>
                )
              )}
            </NativeSelect>
            <FilterInput
              label={t('Start timestamp')}
              value={draft.start_timestamp}
              onChange={(value) => updateFilter('start_timestamp', value)}
            />
            <FilterInput
              label={t('End timestamp')}
              value={draft.end_timestamp}
              onChange={(value) => updateFilter('end_timestamp', value)}
            />
            <FilterInput
              label={t('Error code')}
              value={draft.error}
              onChange={(value) => updateFilter('error', value)}
            />
            <div className='flex gap-2'>
              <Button
                type='button'
                onClick={() => {
                  setFilters(draft)
                  setPage(1)
                }}
              >
                {t('Apply')}
              </Button>
              <Button
                type='button'
                variant='outline'
                onClick={() => {
                  setDraft(EMPTY_FILTERS)
                  setFilters(EMPTY_FILTERS)
                  setPage(1)
                }}
              >
                {t('Reset')}
              </Button>
            </div>
          </div>
          <div className='rounded-xl border'>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>{t('Task')}</TableHead>
                  <TableHead>{t('Provider')}</TableHead>
                  <TableHead>{t('State')}</TableHead>
                  <TableHead>{t('Billing')}</TableHead>
                  <TableHead>{t('Error')}</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell>{item.id}</TableCell>
                    <TableCell className='max-w-48 truncate font-mono'>
                      {item.public_task_id}
                    </TableCell>
                    <TableCell>
                      {item.provider} / #{item.channel_id}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          item.state === 'UNKNOWN' ? 'destructive' : 'outline'
                        }
                      >
                        {item.state}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          item.billing_state === 'REFUNDED'
                            ? 'secondary'
                            : 'outline'
                        }
                      >
                        {item.billing_state}
                      </Badge>
                    </TableCell>
                    <TableCell>{item.error_code || '—'}</TableCell>
                    <TableCell>
                      <Button
                        size='sm'
                        variant='outline'
                        onClick={() => setSelectedId(item.id)}
                      >
                        {t('View')}
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
                {!listQuery.isLoading && items.length === 0 && (
                  <TableRow>
                    <TableCell
                      colSpan={7}
                      className='text-muted-foreground py-10 text-center'
                    >
                      {t('No submissions found')}
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
          <div className='flex items-center justify-between text-sm'>
            <span>{t('{{count}} submissions', { count: total })}</span>
            <div className='flex gap-2'>
              <Button
                size='sm'
                variant='outline'
                disabled={page <= 1}
                onClick={() => setPage((value) => value - 1)}
              >
                {t('Previous')}
              </Button>
              <Button
                size='sm'
                variant='outline'
                disabled={page * pageSize >= total}
                onClick={() => setPage((value) => value + 1)}
              >
                {t('Next')}
              </Button>
            </div>
          </div>
        </div>
        <SubmissionDetailDialog
          submission={detail}
          open={selectedId !== null && !reconcileOpen}
          onOpenChange={(open) => {
            if (!open) setSelectedId(null)
          }}
          onReconcile={() => setReconcileOpen(true)}
          canReconcile={isRoot}
        />
        <ReconcileDialog
          submission={detail}
          open={reconcileOpen}
          pending={reconcileMutation.isPending}
          onOpenChange={setReconcileOpen}
          onSubmit={(payload) => reconcileMutation.mutate(payload)}
        />
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}

function FilterInput(props: {
  label: string
  value: string
  onChange: (value: string) => void
}) {
  return (
    <Input
      aria-label={props.label}
      placeholder={props.label}
      value={props.value}
      onChange={(event) => props.onChange(event.target.value)}
    />
  )
}
