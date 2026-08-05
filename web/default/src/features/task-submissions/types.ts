/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/

export type TaskSubmission = {
  id: number
  created_at: number
  updated_at: number
  version: number
  user_id: number
  client_request_id: string
  public_task_id: string
  state: string
  poll_state: string
  billing_state: string
  provider_cost_state: string
  archive_state: string
  provider: string
  channel_id: number
  channel_type: number
  origin_model_name: string
  reserved_quota: number
  billing_source: string
  provider_status?: string
  error_code?: string
  error_message?: string
  send_attempts: number
  poll_attempts: number
}

export type BillingEntry = {
  id: number
  operation: string
  funding_quota: number
  token_quota: number
  billing_state_before: string
  billing_state_after: string
  reason_code?: string
  actor?: string
  created_at: number
}

export type ReconciliationReview = {
  id: number
  expected_version: number
  decision: string
  upstream_task_id?: string
  admin_id: number
  reason: string
  evidence_recorded: boolean
  strong_correlation: boolean
  status: string
  error_code?: string
  created_at: number
}

export type TaskSubmissionDetail = TaskSubmission & {
  request_summary?: string
  public_price_snapshot?: string
  provider_cost_snapshot?: string
  upstream_task_id?: string
  provider_request_id?: string
  billing_entries: BillingEntry[]
  reconciliation_reviews: ReconciliationReview[]
}

export type TaskSubmissionFilters = {
  client_request_id: string
  public_task_id: string
  provider_request_id: string
  upstream_task_id: string
  user_id: string
  provider: string
  channel_id: string
  state: string
  start_timestamp: string
  end_timestamp: string
  error: string
}

export type ReconcilePayload = {
  expected_version: number
  decision: 'not_created' | 'bind'
  evidence: string
  reason: string
  upstream_task_id?: string
  provider_request_id?: string
}

export type ApiResponse<T> = {
  success: boolean
  message?: string
  data?: T
}
