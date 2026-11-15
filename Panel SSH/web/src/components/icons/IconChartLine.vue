<script setup lang="ts">
/**
 * 任务列表图标（折线图）。
 *
 * 素材取自 lucide-animated 的 chart-line：悬停时外框（坐标轴）保持静止，
 * 只有内部折线以描边生长的方式重画一遍，表达"任务进度推进"。
 * 折线带 pathLength="1"，把自身长度归一化，CSS 才能用固定数值的
 * stroke-dasharray 做生长动画，而不必事先量出路径的真实长度。
 * 动画规则见 styles/global.css 的 .icon-chart-line 段。
 */
</script>

<template>
  <svg
    class="icon-chart-line"
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
    <path d="M3 3v16a2 2 0 0 0 2 2h16" />
    <path class="icon-chart-line__trend" pathLength="1" d="m7 13 3-3 4 4 5-5" />
  </svg>
</template>
