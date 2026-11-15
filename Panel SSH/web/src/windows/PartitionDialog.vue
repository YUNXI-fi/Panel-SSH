<script setup lang="ts">
/**
 * 分区管理：分区表、分区与空闲空间。
 *
 * 三件事放在一个弹窗里，是因为它们本来就是一条链：没有分区表就建不了分区，
 * 建分区要先知道哪里是空的。分成三个入口会让用户在两三个弹窗之间来回找。
 *
 * 破坏性操作（重建分区表、删除分区）都走 ConfirmDialog，要求原样输入磁盘路径。
 * 新建分区不毁数据，因此不设确认 —— 把所有操作都套上确认，只会让确认被无视。
 */
import { computed, ref, watch } from 'vue';
import { NAlert, NButton, NInput, NModal, NRadio, NRadioGroup, NSpace, useMessage } from 'naive-ui';

import ConfirmDialog from './ConfirmDialog.vue';
import { ApiError, api, type BlockDevice, type EscalationInfo, type StorageRequest } from '../api/client';
import { formatBytes } from '../api/format';

const props = defineProps<{
  show: boolean;
  hostId: number | null;
  device: BlockDevice | null;
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

const hasParted = computed(() => props.tools.includes('parted'));

/**
 * 这块盘是否不允许任何改动：系统盘，或还有分区挂着。
 *
 * 服务端也会独立拒绝这两种情况，这里提前禁用只是不让用户白填一遍表单 ——
 * 判断的权威仍在服务端，因为快照可能已经比这里看到的新。
 */
const immutable = computed(() => {
  const device = props.device;
  if (!device) return true;
  return device.system || device.busy;
});

// ---- 分区与空闲空间 ----

/**
 * 起始对齐到 1 MiB。
 *
 * 两个原因：GPT 的主分区表要占掉磁盘开头那一段，从 0 开始建会与它重叠；
 * 现代磁盘的 4K 物理扇区也要求分区按 1 MiB 对齐，否则读写会跨扇区。
 */
const ALIGN = 1024 * 1024;

interface FreeRange {
  start: number;
  end: number;
}

const partitions = computed(() =>
  [...(props.device?.children ?? [])].sort((a, b) => a.partNumber - b.partNumber),
);

const freeRanges = computed<FreeRange[]>(() => {
  const device = props.device;
  if (!device) return [];

  const ranges: FreeRange[] = [];
  let cursor = ALIGN;
  for (const part of [...device.children].sort((a, b) => a.start - b.start)) {
    if (part.start > cursor) ranges.push({ start: cursor, end: part.start });
    cursor = Math.max(cursor, part.start + part.size);
  }
  if (device.size > cursor) ranges.push({ start: cursor, end: device.size });

  // 小于一个对齐单位的空隙建不了分区，列出来只会让人白点一次。
  return ranges.filter((range) => range.end - range.start > ALIGN);
});

// ---- 操作 ----

interface Pending {
  title: string;
  summary: string;
  detail: string;
  phrase: string;
  payload: StorageRequest;
}

const pending = ref<Pending | null>(null);
const applying = ref(false);

const partForm = ref({ start: '', end: '' });
const tableType = ref('gpt');

watch(
  () => props.show,
  (on) => {
    if (!on) return;
    pending.value = null;
    partForm.value = { start: '', end: '' };
    tableType.value = 'gpt';
    void loadEscalation();
  },
);

/**
 * 这台主机会怎么提权。
 *
 * 分区、重建分区表都要 root，面板会自动提权（见后端 privilege.go）。
 * 这个弹窗本身就是"动手前的界面"，所以提醒放在弹窗顶部：新建分区没有二次确认，
 * 只放在确认框里就等于漏掉了它。
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

function useRange(range: FreeRange): void {
  // 用字节偏移而不是百分比：百分比是相对整个磁盘算的，
  // 在有分区的情况下很容易落进已有分区里。
  partForm.value.start = `${range.start}B`;
  partForm.value.end = `${range.end}B`;
}

const canCreate = computed(
  () =>
    props.hostId !== null &&
    props.device !== null &&
    props.device.partitionTable !== '' &&
    hasParted.value &&
    !immutable.value &&
    partForm.value.start.trim() !== '' &&
    partForm.value.end.trim() !== '' &&
    !applying.value,
);

function askLabel(): void {
  const device = props.device;
  if (!device) return;
  pending.value = {
    title: '重建分区表',
    summary: `把 ${device.path} 的分区表重建为 ${tableType.value}，原有分区及其数据将全部丢失。`,
    detail: '这一步之后磁盘上不会留下任何分区，需要重新建立分区并格式化才能使用。',
    phrase: device.path,
    payload: { action: 'part-label', device: device.path, partTable: tableType.value },
  };
}

function askDelete(part: BlockDevice): void {
  const device = props.device;
  if (!device) return;
  pending.value = {
    title: '删除分区',
    summary: `删除 ${device.path} 上的分区 ${part.partNumber}（${part.path}）。`,
    detail: part.fstype
      ? `这个分区上的 ${part.fstype} 文件系统与其中的数据会一并丢失。`
      : '这个分区上没有文件系统，删除它不会丢失数据。',
    phrase: device.path,
    payload: { action: 'part-delete', device: device.path, partNumber: part.partNumber },
  };
}

async function confirmPending(): Promise<void> {
  const request = pending.value;
  const hostId = props.hostId;
  if (!request || hostId === null) return;

  applying.value = true;
  try {
    const result = await api.storage.apply(hostId, { ...request.payload, confirm: request.phrase });
    if (!result.verified) {
      message.warning(`${result.summary}：${result.verifyNote ?? '状态复核未通过'}`);
    } else {
      message.success(result.summary);
    }
    pending.value = null;
    partForm.value = { start: '', end: '' };
    emit('done');
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '分区操作失败');
  } finally {
    applying.value = false;
  }
}

async function createPartition(): Promise<void> {
  const device = props.device;
  const hostId = props.hostId;
  if (!device || hostId === null || !canCreate.value) return;

  applying.value = true;
  try {
    const result = await api.storage.apply(hostId, {
      action: 'part-create',
      device: device.path,
      partStart: partForm.value.start.trim(),
      partEnd: partForm.value.end.trim(),
    });
    if (!result.verified) {
      message.warning(`${result.summary}：${result.verifyNote ?? '状态复核未通过'}`);
    } else {
      message.success(result.summary);
    }
    partForm.value = { start: '', end: '' };
    emit('done');
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '新建分区失败');
  } finally {
    applying.value = false;
  }
}
</script>

<template>
  <n-modal v-model:show="visible" preset="card" title="分区管理" style="width: 700px">
    <div v-if="device" class="part">
      <n-alert v-if="escalationNotice" :type="escalationNotice.type">
        {{ escalationNotice.text }}
      </n-alert>

      <dl class="part__facts">
        <div class="part__fact">
          <dt>磁盘</dt>
          <dd class="num">{{ device.path }}</dd>
        </div>
        <div class="part__fact">
          <dt>容量</dt>
          <dd class="num">{{ formatBytes(device.size) }}</dd>
        </div>
        <div class="part__fact">
          <dt>分区表</dt>
          <dd class="num">{{ device.partitionTable || '尚未初始化' }}</dd>
        </div>
      </dl>

      <p v-if="!hasParted" class="part__error">
        目标主机上没有 parted，无法在这里改分区。装好 parted 后重试。
      </p>
      <p v-else-if="device.system" class="part__error">
        这是承载操作系统的磁盘。面板不允许对它做任何分区改动，下面的信息仅供查看。
      </p>
      <p v-else-if="device.busy" class="part__error">
        这块磁盘上还有分区处于挂载状态。要改动分区表，必须先把它们全部卸载。
      </p>

      <!-- 还没有分区表：先初始化，否则后面什么都做不了 -->
      <section v-if="device.partitionTable === ''" class="part__section">
        <header class="part__head">
          <span>初始化分区表</span>
        </header>
        <p class="part__tip">
          这块磁盘还没有分区表。新建分区之前必须先建一个 —— 这一步会清空盘上残留的分区信息。
        </p>
        <div class="part__initrow">
          <n-radio-group v-model:value="tableType" size="small">
            <n-radio value="gpt">GPT（推荐，支持大于 2 TB 的磁盘）</n-radio>
            <n-radio value="msdos">MBR / msdos（需要与老系统共存时）</n-radio>
          </n-radio-group>
          <span class="part__spacer" />
          <n-button
            size="small"
            type="error"
            :disabled="!hasParted || immutable || applying"
            @click="askLabel"
          >
            初始化…
          </n-button>
        </div>
      </section>

      <section class="part__section">
        <header class="part__head">
          <span>现有分区</span>
          <span class="part__note">{{ partitions.length }} 个</span>
        </header>
        <ul v-if="partitions.length > 0" class="part__list">
          <li v-for="part in partitions" :key="part.path" class="part__row">
            <span class="part__idx num">{{ part.partNumber }}</span>
            <span class="part__path num">{{ part.path }}</span>
            <span class="part__size num">{{ formatBytes(part.size) }}</span>
            <span class="part__fs num">{{ part.fstype || '未格式化' }}</span>
            <span class="part__mount num" :title="part.mountPoints.join('、')">
              {{ part.mountPoints.join('、') || '—' }}
            </span>
            <n-button
              size="tiny"
              quaternary
              type="error"
              :disabled="applying || immutable"
              @click="askDelete(part)"
            >
              删除
            </n-button>
          </li>
        </ul>
        <p v-else class="part__tip">这块磁盘上还没有分区。</p>
      </section>

      <section v-if="device.partitionTable !== ''" class="part__section">
        <header class="part__head">
          <span>空闲空间</span>
          <span class="part__note">{{ freeRanges.length }} 段</span>
        </header>
        <ul v-if="freeRanges.length > 0" class="part__ranges">
          <li v-for="range in freeRanges" :key="range.start" class="part__range">
            <span class="num">{{ formatBytes(range.start) }} – {{ formatBytes(range.end) }}</span>
            <span class="part__rangesize num">可用 {{ formatBytes(range.end - range.start) }}</span>
            <n-button size="tiny" quaternary :disabled="applying" @click="useRange(range)">
              用这一段
            </n-button>
          </li>
        </ul>
        <p v-else class="part__tip">没有可用的空闲空间。</p>

        <div class="part__newrow">
          <label class="part__field">
            <span>起始</span>
            <n-input v-model:value="partForm.start" size="small" placeholder="1048576B" />
          </label>
          <label class="part__field">
            <span>结束</span>
            <n-input v-model:value="partForm.end" size="small" placeholder="100%" />
          </label>
          <n-button
            size="small"
            type="primary"
            :disabled="!canCreate"
            :loading="applying"
            @click="createPartition"
          >
            新建分区
          </n-button>
        </div>
        <p class="part__tip">
          刚建好的分区还没有文件系统，需要在设备列表里对它做一次「格式化」才能挂载。
        </p>
      </section>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button @click="visible = false">关闭</n-button>
      </n-space>
    </template>

    <confirm-dialog
      :show="pending !== null"
      :title="pending?.title ?? ''"
      :summary="pending?.summary ?? ''"
      :detail="pending?.detail"
      :phrase="pending?.phrase ?? ''"
      :loading="applying"
      @update:show="(value: boolean) => { if (!value) pending = null; }"
      @confirm="confirmPending"
    />
  </n-modal>
</template>
