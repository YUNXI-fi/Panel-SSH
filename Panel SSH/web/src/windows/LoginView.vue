<script setup lang="ts">
import { ref } from 'vue';
import { NButton, NForm, NFormItem, NInput, useMessage } from 'naive-ui';

import { ApiError, api, type SessionUser } from '../api/client';

const emit = defineEmits<{ (e: 'authenticated', user: SessionUser): void }>();
const message = useMessage();

const username = ref('admin');
const password = ref('');
const loading = ref(false);

async function submit(): Promise<void> {
  // 按钮是 submit 类型，回车与点击都会走到这里。没有这道闸，
  // 连按回车会并发发出多次登录请求，结果互相覆盖。
  if (loading.value) return;

  if (!username.value || !password.value) {
    message.warning('请输入用户名与密码');
    return;
  }

  loading.value = true;
  try {
    const user = await api.auth.login(username.value, password.value);
    emit('authenticated', user);
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '登录失败，请检查网络连接');
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <div class="login">
    <main class="login__panel">
      <header class="login__brand">
        <span class="login__mark" aria-hidden="true">&gt;_</span>
        <div class="login__brand-text">
          <h1 class="login__title">Panel SSH</h1>
          <p class="login__sub">Go 后端 · 网页桌面 · 轻量服务器运维</p>
        </div>
      </header>

      <!--
        用真正的表单提交而不是给每个输入框挂 keyup.enter：
        前者是原生语义，回车、屏幕阅读器、密码管理器都认这一套。

        aria-label 不是冗余：naive-ui 把 label 渲染成 div 而非 <label for>，
        屏幕阅读器读不到字段名，因此显式标注一次。
      -->
      <n-form class="login__form" :model="{ username, password }" @submit.prevent="submit">
        <n-form-item label="用户名" :show-feedback="false">
          <n-input
            v-model:value="username"
            placeholder="用户名"
            :input-props="{ autocomplete: 'username', 'aria-label': '用户名' }"
          />
        </n-form-item>

        <n-form-item label="密码" :show-feedback="false">
          <n-input
            v-model:value="password"
            type="password"
            show-password-on="click"
            placeholder="密码"
            :input-props="{ autocomplete: 'current-password', 'aria-label': '密码' }"
          />
        </n-form-item>

        <n-button
          class="login__submit"
          type="primary"
          block
          attr-type="submit"
          :loading="loading"
        >
          登录
        </n-button>
      </n-form>

      <p class="login__tip">首次启动的初始密码打印在服务端日志中。</p>
    </main>
  </div>
</template>
