<script setup lang="ts">
/**
 * 磁盘健康报告（SMART）。
 *
 * 这是一个只读面板，但在目标机上它是一次实打实的 IO：smartctl 会把休眠中的盘
 * 叫醒。所以只在用户主动打开时才去读，绝不跟着 5 秒一轮的轮询跑。
 *
 * 展示上只给关键项和已经异常的项。一块普通硬盘有三十多条属性，全部铺开的结果
 * 是真正要看的信号（重分配扇区涨了没）被淹掉，而不是"信息更完整"。
 */
import { computed, ref, watch } from 'vue';
import { NButton, NModal, NSpace } from 'naive-ui';

import { ApiError, api, type SmartAttribute, type SmartReport } from '../api/client';

const props = defineProps<{
  show: boolean;
  hostId: number | null;
  /** 要检查的磁盘路径。分区没有独立的 SMART，所以这里始终是整盘。 */
  device: string;
}>();

const emit = defineEmits<{ (e: 'update:show', value: boolean): void }>();

const visible = computed({
  get: () => props.show,
  set: (value) => emit('update:show', value),
});

const report = ref<SmartReport | null>(null);
const loading = ref(false);
const error = ref('');

async function load(): Promise<void> {
  if (props.hostId === null || props.device === '') return;

  loading.value = true;
  error.value = '';
  try {
    report.value = await api.storage.smart(props.hostId, props.device);
  } catch (err) {
    report.value = null;
    error.value = err instanceof ApiError ? err.message : '读取 SMART 信息失败';
  } finally {
    loading.value = false;
  }
}

watch(
  () => props.show,
  (on) => {
    if (on) void load();
  },
);

/** 温度与通电时长缺失时给占位符，而不是 0 —— 0 °C 会被当成一个真实读数。 */
const temperatureText = computed(() => {
  const value = report.value?.temperature ?? -1;
  return value >= 0 ? `${value} °C` : '—';
});

const powerOnText = computed(() => {
  const hours = report.value?.powerOnHours ?? -1;
  return hours >= 0 ? `${hours.toLocaleString('zh-CN')} 小时` : '—';
});

function rawText(attr: SmartAttribute): string {
  return attr.unit === '' ? String(attr.raw) : `${attr.raw} ${attr.unit}`;
}

/**
 * ATA 属性的"当前值 / 最差值 / 阈值"。
 *
 * NVMe 没有这套归一化读数（它的指标本身就是裸计数），此时三个值都是 0，
 * 显示成 "0 / 0 / 0" 会让人以为阈值是 0，不如直接给占位符。
 */
function scoreText(attr: SmartAttribute): string {
  if (attr.value === 0 && attr.worst === 0 && attr.threshold === 0) return '—';
  return `${attr.value} / ${attr.worst} / ${attr.threshold}`;
}
</script>

<template>
  <n-modal v-model:show="visible" preset="card" title="磁盘健康" style="width: 660px">
    <div class="smart">
      <p class="smart__device num">{{ device }}</p>

      <p v-if="loading" class="smart__hint">正在读取 SMART 信息…</p>
      <p v-else-if="error" class="smart__error">{{ error }}</p>
      <p v-else-if="report && !report.available" class="smart__hint">
        {{ report.note || '这块设备没有报告 SMART 信息' }}
      </p>

      <template v-else-if="report">
        <p class="smart__verdict" :class="report.passed ? 'is-ok' : 'is-bad'">
          <span class="led" :class="report.passed ? 'led--ok' : 'led--danger'" aria-hidden="true" />
          {{ report.passed ? 'SMART 判定健康' : 'SMART 判定异常，建议尽快备份并更换' }}
        </p>

        <dl class="smart__facts">
          <div class="smart__fact">
            <dt>型号</dt>
            <dd class="num">{{ report.model || '—' }}</dd>
          </div>
          <div class="smart__fact">
            <dt>序列号</dt>
            <dd class="num">{{ report.serial || '—' }}</dd>
          </div>
          <div class="smart__fact">
            <dt>温度</dt>
            <dd class="num">{{ temperatureText }}</dd>
          </div>
          <div class="smart__fact">
            <dt>通电时长</dt>
            <dd class="num">{{ powerOnText }}</dd>
          </div>
        </dl>

        <table v-if="report.attributes.length > 0" class="smart__table">
          <thead>
            <tr>
              <th>属性</th>
              <th>当前 / 最差 / 阈值</th>
              <th>原始值</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="attr in report.attributes"
              :key="attr.name"
              :class="{ 'is-failed': attr.failed }"
            >
              <td>
                <span class="num">{{ attr.name }}</span>
                <span v-if="attr.failed" class="smart__tag">已超阈值</span>
              </td>
              <td class="num">{{ scoreText(attr) }}</td>
              <td class="num">{{ rawText(attr) }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="smart__hint">这块设备没有提供可解读的属性项</p>

        <p class="smart__note">
          只列出与数据完整性相关的属性和已跌破阈值的项。要看完整属性，
          请在目标机上直接运行 smartctl。
        </p>
      </template>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button :loading="loading" @click="load">重新读取</n-button>
        <n-button type="primary" @click="visible = false">关闭</n-button>
      </n-space>
    </template>
  </n-modal>
</template>
