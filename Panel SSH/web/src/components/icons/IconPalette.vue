<script setup lang="ts">
/**
 * 主题中心图标：调色板。
 *
 * 与 railbar 其它图标保持同一网格（24）与描边（2），
 * 缩放到 16px 后视觉粗细一致。
 */
</script>

<template>
  <svg
    width="16"
    height="16"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path
      d="M12 3a9 9 0 1 0 0 18c1.1 0 2-.9 2-2 0-.6-.2-1.1-.6-1.5-.4-.4-.6-.9-.6-1.5 0-1.1.9-2 2-2h2a4 4 0 0 0 4-4c0-3.9-3.9-7-8.8-7Z"
    />
    <circle cx="7.5" cy="11" r="1" />
    <circle cx="10" cy="7.5" r="1" />
    <circle cx="14.5" cy="7.5" r="1" />
    <circle cx="17" cy="11" r="1" />
  </svg>
</template>
