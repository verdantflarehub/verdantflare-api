/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/

import { api } from '@/lib/api'

import type {
  ApiResponse,
  ReconcilePayload,
  TaskSubmission,
  TaskSubmissionDetail,
  TaskSubmissionFilters,
} from './types'

export async function getTaskSubmissions(
  filters: TaskSubmissionFilters,
  page: number,
  pageSize: number
): Promise<ApiResponse<{ items: TaskSubmission[]; total: number }>> {
  const params = new URLSearchParams({
    p: String(page),
    page_size: String(pageSize),
  })
  Object.entries(filters).forEach(([key, value]) => {
    if (value) params.set(key, value)
  })
  const response = await api.get(`/api/task-submissions?${params.toString()}`)
  return response.data
}

export async function getTaskSubmission(
  id: number
): Promise<ApiResponse<TaskSubmissionDetail>> {
  const response = await api.get(`/api/task-submissions/${id}`)
  return response.data
}

export async function reconcileTaskSubmission(
  id: number,
  payload: ReconcilePayload
): Promise<
  ApiResponse<{
    pending_review: boolean
    reviewer_count: number
    requires_evidence?: boolean
  }>
> {
  const response = await api.post(
    `/api/task-submissions/${id}/reconcile`,
    payload
  )
  return response.data
}
