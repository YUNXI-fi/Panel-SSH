<script setup lang="ts">
/**
 * 通知铃铛图标。
 *
 * 路径与动效素材取自 lucide-animated 的 bell（悬停时以顶部挂点为轴左右摇摆）。
 * 该库以 React + motion 的 .tsx 形式分发，本项目是 Vue，因此这里只取 SVG 路径，
 * 动效用 CSS 复刻 —— 不为四个图标引入一整套 React 运行时。
 * 动画规则见 styles/global.css 的 .icon-bell 段。
 *
 * 网格取 24、描边取 2：缩放到 16px 显示后实际描边约 1.33px，
 * 与 railbar 原有的手绘图标（16 网格 / 描边 1.3）粗细一致，不会显得突兀。
 */
</script>

<template>
  <svg
    class="icon-bell"
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
    <path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9" />
    <path d="M10.3 21a1.94 1.94 0 0 0 3.4 0" />
  </svg>
</template>
