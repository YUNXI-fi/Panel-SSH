<script setup lang="ts">
/**
 * 格式化设备。
 *
 * 这一步之后盘上的数据就没了，所以底部不是一个"确定"按钮，而是一个要求
 * 原样输入设备路径的输入框 —— 让人在按下去之前再看一眼目标是什么。
 *
 * 文件系统列表由服务端给出（snapshot.formatTargets），缺 mkfs 工具的类型
 * 直接禁用并写明原因。否则用户会一路填完、点下按钮，才收到一句
 * "目标主机上没有 mkfs.xfs" —— 那是把一次必然的失败包装成了一次操作。
 */
import { computed, ref, watch } from 'vue';
import { NAlert, NButton, NInput, NModal, NSelect, NSpace, useMessage } from 'naive-ui';

import { ApiError, api, type BlockDevice, type EscalationInfo } from '../api/client';
import { formatBytes } from '../api/format';

const props = defineProps<{
  show: boolean;
  hostId: number | null;
  device: BlockDevice | null;
  /** 文件系统类型 -> mkfs 命令名。 */
  formatTargets: Record<string, string>;
  /** 目标机上存在的命令，用来判断哪些类型真的可用。 */
  tools: string[];
}>();

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void;
  (e: 'done'): void;
}>();

const message = useMessage();

const visible = computed({
  get: () => props.show,
  set: (value) => emit('update:show', value),
});

const fstype = ref('ext4');
const label = ref('');
const typed = ref('');
const submitting = ref(false);
const error = ref('');

const options = computed(() =>
  Object.entries(props.formatTargets).map(([type, tool]) => {
    const available = props.tools.includes(tool);
    return {
      value: type,
      label: available ? type : `${type}（目标机缺 ${tool}）`,
      disabled: !available,
    };
  }),
);

watch(
  () => props.show,
  (on) => {
    if (!on) return;
    typed.value = '';
    label.value = '';
    error.value = '';
    // 默认选第一个真正可用的类型，而不是硬编码 ext4 ——
    // 目标机上可能只装了 mkfs.xfs。
    const first = options.value.find((option) => !option.disabled);
    if (first) fstype.value = first.value;
    void loadEscalation();
  },
);

const matched = computed(() => props.device !== null && typed.value.trim() === props.device.path);
const canSubmit = computed(() => matched.value && !submitting.value);

/**
 * 这台主机会怎么提权。
 *
 * 格式化必须 root，面板会自动提权（见后端 privilege.go），
 * 所以要在按下按钮之前告诉用户一声；目标机根本提不了权时更要提前说，
 * 否则用户会在输入完设备路径、按下按钮之后才收到失败。
 */
const escalation = ref<EscalationInfo | null>(null);

async function loadEscalation(): Promise<void> {
  const id = props.hostId;
  escalation.value = null;
  if (id === null) return;
  try {
    escalation.value = await api.hosts.escalation(id);
  } catch {
    // 静默：提醒是辅助信息，取不到就不显示，不该因此打断操作。
  }
}

const escalationNotice = computed<{ type: 'info' | 'error'; text: string } | null>(() => {
  const info = escalation.value;
  // 本来就是 root 登录：没有额外信息可提醒。
  if (!info || info.mode === 'root') return null;
  if (!info.escalatable) {
    return {
      type: 'error',
      text: '该主机无法提权，而此操作需要 root：目标机既不是 root 登录，也没有可用的提权路径（sudo/su）。请在面板里为该主机配置提权口令（root 口令，走 su），或以 root 登录后再试。',
    };
  }
  return {
    type: 'info',
    text: `本次操作会以 root 身份执行：面板将自动提权（${info.label}）。`,
  };
});

async function submit(): Promise<void> {
  const device = props.device;
  if (!device || props.hostId === null || !canSubmit.value) return;

  submitting.value = true;
  error.value = '';
  try {
    const result = await api.storage.apply(props.hostId, {
      action: 'format',
      device: device.path,
      fstype: fstype.value,
      label: label.value.trim(),
      confirm: device.path,
    });

    if (!result.verified) {
      // 命令跑完了但状态不对：既不能说成功，也不该说失败，把复核结论原样带出去。
      message.warning(`${result.summary}：${result.verifyNote ?? '状态复核未通过'}`);
    } else {
      message.success(result.summary);
    }
    visible.value = false;
    emit('done');
  } catch (err) {
    // 失败原因留在弹窗里而不是弹一条消息：用户要看着它决定下一步做什么。
    error.value = err instanceof ApiError ? err.message : '格式化失败';
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <n-modal v-model:show="visible" preset="card" title="格式化设备" style="width: 580px">
    <div v-if="device" class="fmt">
      <dl class="fmt__facts">
        <div class="fmt__fact">
          <dt>设备</dt>
          <dd class="num">{{ device.path }}</dd>
        </div>
        <div class="fmt__fact">
          <dt>容量</dt>
          <dd class="num">{{ formatBytes(device.size) }}</dd>
        </div>
        <div class="fmt__fact">
          <dt>当前文件系统</dt>
          <dd class="num">{{ device.fstype || '（尚未格式化）' }}</dd>
        </div>
        <div v-if="device.model" class="fmt__fact">
          <dt>型号</dt>
          <dd class="num">{{ device.model }}</dd>
        </div>
      </dl>

      <n-alert v-if="escalationNotice" :type="escalationNotice.type">
        {{ escalationNotice.text }}
      </n-alert>

      <p class="fmt__danger">格式化会清空这块设备上的全部数据，且无法撤销。</p>

      <div class="fmt__field">
        <span class="fmt__label">文件系统</span>
        <n-select v-model:value="fstype" size="small" :options="options" />
      </div>
      <div class="fmt__field">
        <span class="fmt__label">卷标</span>
        <n-input
          v-model:value="label"
          size="small"
          placeholder="可留空；字母、数字、空格与 . _ -"
        />
      </div>

      <label class="fmt__ask">
        请输入 <strong class="num">{{ device.path }}</strong> 以确认：
        <n-input
          v-model:value="typed"
          class="fmt__confirm"
          :placeholder="device.path"
          :input-props="{ 'aria-label': '确认输入', autocomplete: 'off' }"
        />
      </label>

      <p v-if="error" class="fmt__error">{{ error }}</p>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button @click="visible = false">取消</n-button>
        <n-button type="error" :disabled="!canSubmit" :loading="submitting" @click="submit">
          格式化
        </n-button>
      </n-space>
    </template>
  </n-modal>
</template>
