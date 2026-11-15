<script setup lang="ts">
import { NDropdown } from 'naive-ui';

/**
 * 文件拖拽落下后的小菜单。
 *
 * 为什么不直接按默认动作执行：跨主机移动是"复制成功后再删源"，
 * 同一手势的两种后果差别太大，松手后多问一句比事后补救便宜。
 * 点菜单外面即取消，不会误触发任何动作。
 */
defineProps<{ show: boolean; x: number; y: number }>();

const emit = defineEmits<{
  (event: 'select', action: 'copy' | 'move'): void;
  (event: 'update:show', value: boolean): void;
}>();

const options = [
  { label: '复制到此处', key: 'copy' },
  { label: '移动到此处', key: 'move' },
];
</script>

<template>
  <n-dropdown
    :show="show"
    :x="x"
    :y="y"
    :options="options"
    trigger="manual"
    placement="bottom-start"
    @select="(key) => emit('select', key as 'copy' | 'move')"
    @clickoutside="emit('update:show', false)"
  />
</template>
