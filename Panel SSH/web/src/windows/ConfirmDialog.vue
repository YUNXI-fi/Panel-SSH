<script setup lang="ts">
/**
 * 破坏性操作的确认框。
 *
 * 要求用户把目标设备路径原样敲一遍，而不是点一下"确定"。理由很直接：
 * 这类操作没有回滚，而磁盘工具最典型的事故就是"在错误的设备上按下了确认"。
 * 敲一遍路径是把注意力拉回目标本身，不是仪式感。
 *
 * 服务端会独立校验这个字段（sysinfo.confirmMatches）—— 前端这道只是让人
 * 停一下，不能替代它，也不该是唯一的一道。
 */
import { computed, ref, watch } from 'vue';
import { NButton, NInput, NModal, NSpace } from 'naive-ui';

const props = defineProps<{
  show: boolean;
  title: string;
  /** 一句话说明将要发生什么，以警示样式呈现。 */
  summary: string;
  /** 必须原样输入的文本。 */
  phrase: string;
  /** 补充说明（可选）。 */
  detail?: string;
  loading?: boolean;
}>();

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void;
  (e: 'confirm'): void;
}>();

const typed = ref('');

const visible = computed({
  get: () => props.show,
  set: (value) => emit('update:show', value),
});

// 每次打开都清空：留着上一次的输入会把"确认"变成肌肉记忆，
// 而那正是这类事故的成因。
watch(
  () => props.show,
  (on) => {
    if (on) typed.value = '';
  },
);

const matched = computed(() => typed.value.trim() === props.phrase);

function submit(): void {
  if (matched.value) emit('confirm');
}
</script>

<template>
  <n-modal v-model:show="visible" preset="card" :title="title" style="width: 540px">
    <div class="confirm">
      <p class="confirm__summary">{{ summary }}</p>
      <p v-if="detail" class="confirm__detail">{{ detail }}</p>

      <label class="confirm__ask">
        请输入 <strong class="num">{{ phrase }}</strong> 以确认：
        <n-input
          v-model:value="typed"
          class="confirm__input"
          :placeholder="phrase"
          :input-props="{ 'aria-label': '确认输入', autocomplete: 'off' }"
          @keyup.enter="submit"
        />
      </label>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button @click="visible = false">取消</n-button>
        <n-button type="error" :disabled="!matched" :loading="loading" @click="submit">
          确认执行
        </n-button>
      </n-space>
    </template>
  </n-modal>
</template>
