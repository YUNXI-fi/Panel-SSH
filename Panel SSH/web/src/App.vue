<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import {
  NConfigProvider,
  NDialogProvider,
  NMessageProvider,
  darkTheme,
  dateZhCN,
  zhCN,
} from 'naive-ui';

import Desktop from './desktop/Desktop.vue';
import LoginView from './windows/LoginView.vue';
import { api, type SessionUser } from './api/client';
import { currentTheme, naiveOverridesFor } from './desktop/theme';

const user = ref<SessionUser | null>(null);
const ready = ref(false);

/**
 * naive-ui 的主题跟随「主题中心」。
 *
 * 颜色值统一由 theme.ts 从同一套调色板派生：深色主题走 naive 的 darkTheme，
 * 浅色主题走默认亮色主题，再用 overrides 把面板特有的表面/描边/文字压回来。
 * 这样切换主题时既不用重建组件树，也不会出现"CSS 已经是浅色、弹窗还是深色"。
 */
const naiveTheme = computed(() => (currentTheme.value.dark ? darkTheme : null));
const themeOverrides = computed(() => naiveOverridesFor(currentTheme.value));

// 先用 /api/auth/me 探测既有会话，避免刷新页面后被踢回登录页。
onMounted(async () => {
  try {
    user.value = await api.auth.me();
  } catch {
    user.value = null;
  } finally {
    ready.value = true;
  }
});
</script>

<template>
  <n-config-provider
    :theme="naiveTheme"
    :theme-overrides="themeOverrides"
    :locale="zhCN"
    :date-locale="dateZhCN"
  >
    <n-message-provider>
      <n-dialog-provider>
        <div v-if="!ready" class="boot">
          <div class="boot__line">
            <span class="led led--sig led--live" aria-hidden="true" />
            <span>PANEL SSH · 正在建立控制台</span>
          </div>
        </div>
        <LoginView v-else-if="!user" @authenticated="user = $event" />
        <Desktop v-else :user="user" @logout="user = null" />
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>
