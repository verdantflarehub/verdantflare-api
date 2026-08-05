/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import React, { useEffect, useMemo, useState } from 'react';
import {
  Button,
  Card,
  Form,
  Modal,
  Pagination,
  Space,
  Table,
  Tag,
  Typography,
} from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';
import { API, isRoot, showError, showSuccess } from '../../helpers';

const EMPTY_FILTERS = {
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
};

const PAGE_SIZE = 20;

const JsonSnapshot = ({ title, value }) => (
  <div className='mb-4'>
    <Typography.Text strong>{title}</Typography.Text>
    <pre className='mt-2 max-h-48 overflow-auto rounded-lg bg-semi-color-fill-0 p-3 text-xs'>
      {value || '—'}
    </pre>
  </div>
);

const TaskSubmission = () => {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(EMPTY_FILTERS);
  const [filters, setFilters] = useState(EMPTY_FILTERS);
  const [items, setItems] = useState([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(false);
  const [detail, setDetail] = useState(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [reconcileVisible, setReconcileVisible] = useState(false);
  const [reconciling, setReconciling] = useState(false);
  const [reconcile, setReconcile] = useState({
    decision: 'not_created',
    reason: '',
    evidence: '',
    upstream_task_id: '',
    provider_request_id: '',
  });

  const loadList = async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams({
        p: String(page),
        page_size: String(PAGE_SIZE),
      });
      Object.entries(filters).forEach(([key, value]) => {
        if (value) params.set(key, value);
      });
      const response = await API.get(`/api/task-submissions?${params}`);
      if (!response.data?.success) {
        showError(response.data?.message || t('加载提交记录失败'));
        return;
      }
      setItems(response.data.data?.items || []);
      setTotal(response.data.data?.total || 0);
    } catch (error) {
      showError(error.message || t('加载提交记录失败'));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadList();
  }, [filters, page]);

  const loadDetail = async (id) => {
    setDetailLoading(true);
    try {
      const response = await API.get(`/api/task-submissions/${id}`);
      if (!response.data?.success) {
        showError(response.data?.message || t('加载提交详情失败'));
        return;
      }
      setDetail(response.data.data);
    } catch (error) {
      showError(error.message || t('加载提交详情失败'));
    } finally {
      setDetailLoading(false);
    }
  };

  const submitReconciliation = async () => {
    if (!detail || !reconcile.reason.trim() || !reconcile.evidence.trim()) {
      showError(t('请填写对账理由和证据'));
      return;
    }
    if (reconcile.decision === 'bind' && !reconcile.upstream_task_id.trim()) {
      showError(t('绑定决策必须填写上游任务 ID'));
      return;
    }
    setReconciling(true);
    try {
      const response = await API.post(
        `/api/task-submissions/${detail.id}/reconcile`,
        {
          expected_version: detail.version,
          ...reconcile,
        },
      );
      if (!response.data?.success) {
        showError(response.data?.message || t('提交对账失败'));
        return;
      }
      if (response.data.data?.requires_evidence) {
        showSuccess(t('复核已记录；执行前仍需供应商结构化证据验证'));
      } else if (response.data.data?.pending_review) {
        showSuccess(t('第一位管理员复核已记录，等待另一位管理员复核'));
      } else {
        showSuccess(t('提交对账完成'));
      }
      setReconcileVisible(false);
      await Promise.all([loadDetail(detail.id), loadList()]);
    } catch (error) {
      showError(
        error.response?.data?.message || error.message || t('提交对账失败'),
      );
    } finally {
      setReconciling(false);
    }
  };

  const columns = useMemo(
    () => [
      { title: 'ID', dataIndex: 'id', width: 80 },
      {
        title: t('任务'),
        render: (_, record) => (
          <div className='max-w-60'>
            <Typography.Text ellipsis={{ showTooltip: true }}>
              {record.public_task_id || record.client_request_id || '—'}
            </Typography.Text>
            <div className='text-xs text-semi-color-text-2'>
              {t('用户')} #{record.user_id}
            </div>
          </div>
        ),
      },
      {
        title: t('供应商 / 渠道'),
        render: (_, record) =>
          `${record.provider || '—'} / #${record.channel_id}`,
      },
      {
        title: t('状态'),
        render: (_, record) => (
          <Tag color={record.state === 'UNKNOWN' ? 'red' : 'blue'}>
            {record.state}
          </Tag>
        ),
      },
      {
        title: t('计费'),
        render: (_, record) => (
          <Tag color={record.billing_state === 'REFUNDED' ? 'green' : 'grey'}>
            {record.billing_state}
          </Tag>
        ),
      },
      {
        title: t('错误码'),
        dataIndex: 'error_code',
        render: (value) => value || '—',
      },
      {
        title: t('操作'),
        render: (_, record) => (
          <Button size='small' onClick={() => loadDetail(record.id)}>
            {t('详情')}
          </Button>
        ),
      },
    ],
    [t],
  );

  return (
    <div className='mt-[60px] px-2'>
      <Card title={t('API Center · 视频提交')}>
        <Form layout='horizontal' className='mb-4'>
          <Form.Input
            field='client_request_id'
            label={t('Client Request ID')}
            value={draft.client_request_id}
            onChange={(value) =>
              setDraft({ ...draft, client_request_id: value })
            }
          />
          <Form.Input
            field='public_task_id'
            label={t('Public Task ID')}
            value={draft.public_task_id}
            onChange={(value) => setDraft({ ...draft, public_task_id: value })}
          />
          <Form.Input
            field='provider_request_id'
            label={t('Provider Request ID')}
            value={draft.provider_request_id}
            onChange={(value) =>
              setDraft({ ...draft, provider_request_id: value })
            }
          />
          <Form.Input
            field='upstream_task_id'
            label={t('上游任务 ID')}
            value={draft.upstream_task_id}
            onChange={(value) =>
              setDraft({ ...draft, upstream_task_id: value })
            }
          />
          <Form.Input
            field='user_id'
            label={t('用户 ID')}
            value={draft.user_id}
            onChange={(value) => setDraft({ ...draft, user_id: value })}
          />
          <Form.Input
            field='provider'
            label={t('供应商')}
            value={draft.provider}
            onChange={(value) => setDraft({ ...draft, provider: value })}
          />
          <Form.Input
            field='channel_id'
            label={t('渠道 ID')}
            value={draft.channel_id}
            onChange={(value) => setDraft({ ...draft, channel_id: value })}
          />
          <Form.Select
            field='state'
            label={t('状态')}
            value={draft.state}
            onChange={(value) => setDraft({ ...draft, state: value || '' })}
            optionList={[
              'PREPARED',
              'SENDING',
              'CONFIRMED',
              'REJECTED',
              'UNKNOWN',
            ].map((value) => ({ label: value, value }))}
            showClear
          />
          <Form.Input
            field='start_timestamp'
            label={t('开始时间戳')}
            value={draft.start_timestamp}
            onChange={(value) => setDraft({ ...draft, start_timestamp: value })}
          />
          <Form.Input
            field='end_timestamp'
            label={t('结束时间戳')}
            value={draft.end_timestamp}
            onChange={(value) => setDraft({ ...draft, end_timestamp: value })}
          />
          <Form.Input
            field='error'
            label={t('错误码')}
            value={draft.error}
            onChange={(value) => setDraft({ ...draft, error: value })}
          />
          <Space>
            <Button
              theme='solid'
              onClick={() => {
                setPage(1);
                setFilters({ ...draft });
              }}
            >
              {t('查询')}
            </Button>
            <Button
              onClick={() => {
                setDraft(EMPTY_FILTERS);
                setFilters(EMPTY_FILTERS);
                setPage(1);
              }}
            >
              {t('重置')}
            </Button>
          </Space>
        </Form>
        <Table
          columns={columns}
          dataSource={items}
          rowKey='id'
          loading={loading}
          pagination={false}
        />
        <div className='mt-4 flex justify-end'>
          <Pagination
            currentPage={page}
            pageSize={PAGE_SIZE}
            total={total}
            onPageChange={setPage}
          />
        </div>
      </Card>

      <Modal
        title={detail ? `${t('提交详情')} #${detail.id}` : t('提交详情')}
        visible={Boolean(detail)}
        width={900}
        footer={
          detail?.state === 'UNKNOWN' && isRoot() ? (
            <Button
              theme='solid'
              type='warning'
              onClick={() => setReconcileVisible(true)}
            >
              {t('人工对账')}
            </Button>
          ) : null
        }
        onCancel={() => setDetail(null)}
        loading={detailLoading}
      >
        {detail && (
          <>
            <Space wrap className='mb-4'>
              <Tag>{detail.state}</Tag>
              <Tag>{detail.billing_state}</Tag>
              <Typography.Text>
                {detail.provider} / #{detail.channel_id}
              </Typography.Text>
              <Typography.Text>
                {t('版本')} {detail.version}
              </Typography.Text>
            </Space>
            <JsonSnapshot
              title={t('请求摘要（已脱敏）')}
              value={detail.request_summary}
            />
            <JsonSnapshot
              title={t('公开价格快照（已脱敏）')}
              value={detail.public_price_snapshot}
            />
            <JsonSnapshot
              title={t('供应商成本快照（已脱敏）')}
              value={detail.provider_cost_snapshot}
            />
            <Typography.Title heading={6}>{t('计费审计')}</Typography.Title>
            <Table
              size='small'
              rowKey='id'
              pagination={false}
              dataSource={detail.billing_entries || []}
              columns={[
                { title: t('动作'), dataIndex: 'operation' },
                { title: t('前状态'), dataIndex: 'billing_state_before' },
                { title: t('后状态'), dataIndex: 'billing_state_after' },
                { title: t('原因'), dataIndex: 'reason_code' },
              ]}
            />
            <Typography.Title heading={6} className='mt-4'>
              {t('对账审计')}
            </Typography.Title>
            <Table
              size='small'
              rowKey='id'
              pagination={false}
              dataSource={detail.reconciliation_reviews || []}
              columns={[
                { title: t('管理员'), dataIndex: 'admin_id' },
                { title: t('决策'), dataIndex: 'decision' },
                { title: t('状态'), dataIndex: 'status' },
                {
                  title: t('证据'),
                  dataIndex: 'evidence_recorded',
                  render: (value) => (value ? t('已记录') : '—'),
                },
                { title: t('理由'), dataIndex: 'reason' },
              ]}
            />
          </>
        )}
      </Modal>

      <Modal
        title={t('人工对账')}
        visible={reconcileVisible}
        confirmLoading={reconciling}
        onOk={submitReconciliation}
        onCancel={() => setReconcileVisible(false)}
      >
        <Form>
          <Form.Select
            field='decision'
            label={t('决策')}
            value={reconcile.decision}
            onChange={(value) =>
              setReconcile({ ...reconcile, decision: value })
            }
            optionList={[
              { label: t('确认上游未创建并退款'), value: 'not_created' },
              { label: t('绑定已创建的上游任务'), value: 'bind' },
            ]}
          />
          {reconcile.decision === 'bind' && (
            <>
              <Form.Input
                field='upstream_task_id'
                label={t('上游任务 ID')}
                value={reconcile.upstream_task_id}
                onChange={(value) =>
                  setReconcile({ ...reconcile, upstream_task_id: value })
                }
              />
              <Form.Input
                field='provider_request_id'
                label={t('Provider Request ID（强关联，可选）')}
                value={reconcile.provider_request_id}
                onChange={(value) =>
                  setReconcile({ ...reconcile, provider_request_id: value })
                }
              />
            </>
          )}
          <Form.TextArea
            field='reason'
            label={t('理由')}
            value={reconcile.reason}
            onChange={(value) => setReconcile({ ...reconcile, reason: value })}
          />
          <Form.TextArea
            field='evidence'
            label={t('证据（仅审计存储，不在详情回显）')}
            value={reconcile.evidence}
            onChange={(value) =>
              setReconcile({ ...reconcile, evidence: value })
            }
          />
        </Form>
      </Modal>
    </div>
  );
};

export default TaskSubmission;
