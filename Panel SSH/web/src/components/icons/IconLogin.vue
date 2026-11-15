<script setup lang="ts">
/**
 * 登录/退出图标（箭头入框）。
 *
 * 素材取自 lucide-animated 的 login：悬停时只有箭头部分向右推一下再回位，
 * 外框静止；用 <g> 包住 polyline 与 line，两者才能作为整体一起位移
 * （分别做动画会因取整差异出现箭头与横线错位）。
 * 动画规则见 styles/global.css 的 .icon-login 段。
 */
</script>

<template>
  <svg
    class="icon-login"
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
    <path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4" />
    <g class="icon-login__arrow">
      <polyline points="10 17 15 12 10 7" />
      <line x1="3" x2="15" y1="12" y2="12" />
    </g>
  </svg>
</template>
