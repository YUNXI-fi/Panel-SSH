<script setup lang="ts">
/**
 * 账户图标（人形），用于"修改密码"——它是账户级操作，不是密码锁本身。
 *
 * 素材取自 lucide-animated 的 user：悬停时头部与肩线先后以描边生长的方式画出，
 * 头部同时从半尺寸放大到位。头部的 transform-origin 必须钉在圆心，
 * 否则会连着位置一起位移（SVG 子元素的 origin 默认在 viewBox 原点）。
 * 动画规则见 styles/global.css 的 .icon-user 段。
 */
</script>

<template>
  <svg
    class="icon-user"
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
    <circle class="icon-user__head" pathLength="1" cx="12" cy="8" r="5" />
    <path class="icon-user__body" pathLength="1" d="M20 21a8 8 0 0 0-16 0" />
  </svg>
</template>
