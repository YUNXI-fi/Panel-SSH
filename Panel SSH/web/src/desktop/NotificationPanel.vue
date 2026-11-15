<script setup lang="ts">
/**
 * 通知中心：右侧滑出的抽屉面板。
 *
 * 做成抽屉而不是窗口，是因为通知是系统级、跨应用的东西 ——
 * 开成窗口就带上了"关掉就看不见了"的语义，而角标必须始终可见。
 *
 * 数据来自 useNotifications 的共享状态，与竖栏铃铛的未读角标同源，
 * 因此面板里的内容与角标上的数字永远一致。
 */
import { computed, inject, ref, watch } from 'vue';
import { NButton, useMessage } from 'naive-ui';

import { ApiError, type Notification } from '../api/client';
import { formatTime } from '../api/format';
import { useNotifications } from '../desktop/useNotifications';
import { WindowManagerKey } from '../desktop/useWindowManager';

const props = defineProps<{ show: boolean }>();
const emit = defineEmits<{
  (e: 'update:show', value: boolean): void;
  (e: 'open-task', taskId: number): void;
}>();

const wm = inject(WindowManagerKey);

const message = useMessage();
const { items, unread, refresh, markRead, markAllRead } = useNotifications();

const busy = ref(false);

const CATEGORY_LABEL: Record<Notification['category'], string> = {
  task: '任务',
  host: '主机',
  security: '管理',
};

/**
 * level 到指示灯配色。
 *
 * info 用琥珀而非灰色：它能出现在通知中心，本身就已经是"需要被看到"的东西；
 * 灰色在这套界面里是"装饰"的语义（--ink-ghost 只用于分隔符与刻度）。
 */
const LED_CLASS: Record<Notification['level'], string> = {
  info: 'led--sig',
  success: 'led--ok',
  warning: 'led--warn',
  error: 'led--danger',
};

const hasUnread = computed(() => unread.value > 0);

function close(): void {
  emit('update:show', false);
}

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function onMarkAll(): Promise<void> {
  busy.value = true;
  try {
    await markAllRead();
  } catch (err) {
    message.error(describe(err, '标记已读失败'));
  } finally {
    busy.value = false;
  }
}

/**
 * 点击一条通知：先标记已读，再跳到能承接这条通知的窗口。
 *
 * 先标已读再跳转 —— 反过来的话，跳转会把面板关掉，
 * 这条通知就永远停在未读状态，角标上的数字再也清不掉。
 *
 * 跳转目标按类别定：任务跳到任务详情，主机跳到主机管理（列表里能看到它
 * 当前是在线还是离线）。管理类通知（改密码、删除主机等）没有可去的页面 ——
 * 它记录的是"已经发生的事"，点开也无处可看。
 */

/** 这条通知点下去有没有地方可去。没有的话渲染成普通行，不给点击暗示。 */
function actionable(item: Notification): boolean {
  if (item.category === 'task') return item.ref > 0;
  return item.category === 'host';
}

/** 已读 + 无目标 = 纯展示行；未读的行仍可点，用来把未读清掉。 */
function isStatic(item: Notification): boolean {
  return item.read && !actionable(item);
}

async function onClick(item: Notification): Promise<void> {
  if (isStatic(item)) return;

  if (!item.read) {
    try {
      await markRead([item.id]);
    } catch (err) {
      message.error(describe(err, '标记已读失败'));
    }
  }

  if (item.category === 'task' && item.ref > 0) {
    emit('open-task', item.ref);
    close();
    return;
  }

  if (item.category === 'host') {
    wm?.open('hosts', '主机管理');
    close();
  }
}

// 面板每次打开都刷新一次：用户点开它的动机就是"现在有什么新东西"，
// 拿一份最长 60 秒前的缓存来回应这个动作，等于让按钮在说谎。
watch(
  () => props.show,
  (show) => {
    if (show) void refresh();
  },
);
</script>

<template>
  <div v-if="show" class="notify">
    <div class="notify__scrim" @click="close" />

    <aside class="notify__panel" role="dialog" aria-label="通知中心">
      <header class="notify__head">
        <span class="silk">通知</span>
        <span v-if="hasUnread" class="notify__count num">{{ unread }}</span>
        <span class="notify__spacer" />
        <n-button size="tiny" :loading="busy" :disabled="!hasUnread" @click="onMarkAll">
          全部已读
        </n-button>
        <button class="notify__close" title="关闭" aria-label="关闭通知中心" @click="close">
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
            <path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" stroke-width="1.2" />
          </svg>
        </button>
      </header>

      <div class="notify__list">
        <p v-if="items.length === 0" class="notify__empty">
          还没有通知。<br />
          批量任务结束、主机上下线，以及改密码与插件装卸等管理操作会在这里留下记录。
        </p>

        <!--
          有目标的通知才是按钮；「已读 + 无目标」的管理类记录渲染成普通行。
          用按钮承载一个点下去什么都不会发生的条目，等于给用户一个假承诺。
        -->
        <component
          :is="isStatic(item) ? 'div' : 'button'"
          v-for="item in items"
          :key="item.id"
          class="notify__item"
          :class="{ 'is-unread': !item.read, 'is-static': isStatic(item) }"
          @click="onClick(item)"
        >
          <span class="led" :class="LED_CLASS[item.level]" aria-hidden="true" />
          <span class="notify__main">
            <span class="notify__title">{{ item.title }}</span>
            <span v-if="item.body" class="notify__body">{{ item.body }}</span>
            <span class="notify__meta">
              <span class="notify__tag">{{ CATEGORY_LABEL[item.category] }}</span>
              <span class="num">{{ formatTime(item.createdAt) }}</span>
              <span v-if="item.category === 'task' && item.ref > 0" class="notify__go">
                查看任务
              </span>
              <span v-else-if="item.category === 'host'" class="notify__go">查看主机</span>
            </span>
          </span>
        </component>
      </div>
    </aside>
  </div>
</template>
