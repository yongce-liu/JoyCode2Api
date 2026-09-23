import { useState } from 'react';
import { Button, Form, Input, Modal, message } from 'antd';
import { KeyOutlined } from '@ant-design/icons';
import { api } from '../api';

interface Props {
  userId: string;
  onSaved: () => void;
}

export default function CustomTokenButton({ userId, onSaved }: Props) {
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [form] = Form.useForm<{ api_token: string }>();

  const save = async ({ api_token }: { api_token: string }) => {
    setSaving(true);
    try {
      await api.updateToken(userId, api_token);
      message.success('Token 已保存，请同步更新客户端配置');
      setOpen(false);
      form.resetFields();
      onSaved();
    } catch (error) {
      message.error(error instanceof Error ? error.message : '保存 Token 失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <span onClick={event => event.stopPropagation()}>
      <Button size="small" icon={<KeyOutlined />} onClick={() => setOpen(true)}>自定义 Token</Button>
      <Modal
        title="自定义 API Token"
        open={open}
        okText="保存"
        cancelText="取消"
        confirmLoading={saving}
        closable={!saving}
        maskClosable={!saving}
        cancelButtonProps={{ disabled: saving }}
        onOk={() => form.submit()}
        onCancel={() => { setOpen(false); form.resetFields(); }}
      >
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item
            name="api_token"
            label="新 Token"
            extra="支持 1–256 位字母、数字及 . _ ~ -。保存后请更新客户端 Token；随机重置功能仍可使用。"
            rules={[
              { required: true, message: '请输入 Token' },
              { pattern: /^[A-Za-z0-9._~-]{1,256}$/, message: 'Token 格式不正确' },
              { validator: (_, value) => ['joycode', 'default'].includes(value)
                ? Promise.reject(new Error('不能使用保留值 joycode 或 default')) : Promise.resolve() },
            ]}
          >
            <Input.Password autoComplete="new-password" maxLength={256} disabled={saving} placeholder="例如 sk-my-custom-token" />
          </Form.Item>
        </Form>
      </Modal>
    </span>
  );
}
