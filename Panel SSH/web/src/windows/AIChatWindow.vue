<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';
import { NButton, NInput, NModal, NPopconfirm, NSpace, NTag, useMessage } from 'naive-ui';

import {
  ApiError,
  aiChatURL,
  api,
  type AIChatHistory,
  type AIConfig,
  type AIMessage,
  type AISession,
} from '../api/client';
import { formatToolArgs, renderMarkdown } from '../markdown';

const message = useMessage();

type Item =
  | { kind: 'user'; text: string }
  | { kind: 'assistant'; text: string }
  | { kind: 'error'; text: string }
  | {
      kind: 'tool';
      callId: string;
      name: string;
      args: string;
      status: 'running' | 'ok' | 'failed';
      output: string;
      expanded: boolean;
    }
  | {
      kind: 'confirm';
      callId: string;
      command: string;
      reason: string;
      state: 'pending' | 'approved' | 'rejected' | 'expired';
    };

const items = ref<Item[]>([]);
const draft = ref('');
const running = ref(false);
const scroller = ref<HTMLElement | null>(null);

/** 全部会话，按最近活动倒序。 */
const sessions = ref<AISession[]>([]);
/**
 * 当前会话。
 *
 * null 表示还没落库的"新对话"草稿：用户真正发出第一句话时才创建会话。
 * 这样反复点"新对话"不会在列表里留下一串空会话。
 */
const activeSessionId = ref<number | null>(null);

const config = ref<AIConfig>({ baseUrl: '', model: '', hasApiKey: false, maxSteps: 200 });
const configLoaded = ref(false);
const showSettings = ref(false);
const settingsDraft = ref({ baseUrl: '', model: '', apiKey: '', maxSteps: 200 });
const saving = ref(false);
const testing = ref(false);

const configured = computed(() => config.value.baseUrl !== '' && config.value.model !== '');

let socket: WebSocket | null = null;
/** 当前 socket 绑定的会话。切换会话时必须换一条连接，不能复用它。 */
let socketSessionId: number | null = null;

const TOOL_LABELS: Record<string, string> = {
  list_hosts: '列出主机',
  run_command: '执行命令',
  get_metrics: '读取运行指标',
  list_processes: '读取进程列表',
};

function toolLabel(name: string): string {
  return TOOL_LABELS[name] ?? name;
}

function describe(error: unknown, fallback: string): string {
  if (error instanceof ApiError) {
    return error.status === 401 ? '会话已过期，请重新登录' : error.message;
  }
  return fallback;
}

async function scrollToBottom(): Promise<void> {
  await nextTick();
  const element = scroller.value;
  if (element) element.scrollTop = element.scrollHeight;
}

async function loadConfig(): Promise<void> {
  try {
    config.value = await api.ai.getConfig();
    settingsDraft.value = {
      baseUrl: config.value.baseUrl,
      model: config.value.model,
      apiKey: '',
      maxSteps: config.value.maxSteps,
    };
  } catch (error) {
    message.error(describe(error, '读取模型配置失败'));
  } finally {
    configLoaded.value = true;
  }
}

async function saveConfig(): Promise<void> {
  saving.value = true;
  try {
    // 数字输入框被清空时值为 null，这里兜底回默认值，避免把 null 发给后端。
    const payload = { ...settingsDraft.value, maxSteps: settingsDraft.value.maxSteps ?? 200 };
    config.value = await api.ai.setConfig(payload);
    // Key 留空表示保持原值，保存后清掉输入框，避免看起来像刚设过。
    settingsDraft.value.apiKey = '';
    showSettings.value = false;
    message.success('模型配置已保存');
  } catch (error) {
    message.error(describe(error, '保存配置失败'));
  } finally {
    saving.value = false;
  }
}

async function testConfig(): Promise<void> {
  testing.value = true;
  try {
    const result = await api.ai.test();
    if (result.ok) message.success(result.message);
    else message.error(result.message);
  } catch (error) {
    message.error(describe(error, '测试连接失败'));
  } finally {
    testing.value = false;
  }
}

function connect(sessionId: number): WebSocket {
  // 复用只发生在同一个会话上：一条连接只认一个会话，换了会话必须新建 ——
  // 服务端的历史、待确认命令、工具状态都挂在连接上。
  if (socket && socketSessionId === sessionId && socket.readyState === WebSocket.OPEN) {
    return socket;
  }
  disconnect();

  const ws = new WebSocket(aiChatURL(sessionId));
  socket = ws;
  socketSessionId = sessionId;
  ws.onmessage = (event: MessageEvent<string>) => {
    let payload: AIMessage;
    try {
      payload = JSON.parse(event.data) as AIMessage;
    } catch {
      return;
    }
    handle(payload);
  };
  ws.onclose = () => {
    // 只处理当前连接：切换会话时旧连接的 close 不该改动界面状态。
    if (socket !== ws) return;
    running.value = false;
    // 服务端主动断开（会话被删除、面板退出）时，界面上的数据可能已经过期。
    void resyncSessions();
  };
  return ws;
}

/**
 * 断开当前连接。
 *
 * 切换会话、删除会话、清空对话之前都必须走这里：服务端按连接持有历史，
 * 不断开的话下一个会话会沿着旧连接继续用上一个会话的上下文。
 */
function disconnect(): void {
  const current = socket;
  socket = null;
  socketSessionId = null;
  running.value = false;
  if (!current) return;

  // 先摘回调再关闭：否则旧连接迟到的消息会写进刚切换过来的消息流。
  current.onmessage = null;
  current.onclose = null;
  current.onopen = null;
  if (current.readyState === WebSocket.OPEN || current.readyState === WebSocket.CONNECTING) {
    current.close();
  }
}

function handle(payload: AIMessage): void {
  switch (payload.type) {
    case 'assistant':
      items.value.push({ kind: 'assistant', text: payload.content ?? '' });
      break;

    case 'tool':
      items.value.push({
        kind: 'tool',
        callId: payload.callId ?? '',
        name: payload.name ?? '',
        args: payload.args ?? '',
        status: 'running',
        output: '',
        expanded: false,
      });
      break;

    case 'toolResult': {
      const target = items.value.find(
        (item): item is Extract<Item, { kind: 'tool' }> =>
          item.kind === 'tool' && item.callId === payload.callId,
      );
      if (target) {
        target.status = payload.ok ? 'ok' : 'failed';
        target.output = payload.output ?? '';
      }
      break;
    }

    case 'confirm':
      items.value.push({
        kind: 'confirm',
        callId: payload.callId ?? '',
        command: payload.args ?? '',
        reason: payload.reason ?? '',
        state: 'pending',
      });
      break;

    case 'confirmExpired': {
      // 服务端已停止等待：此时再点"允许"不会有人接收，
      // 必须把卡片置为过期态而不是让它看起来还能点。
      const target = items.value.find(
        (item): item is Extract<Item, { kind: 'confirm' }> =>
          item.kind === 'confirm' && item.callId === payload.callId,
      );
      if (target && target.state === 'pending') target.state = 'expired';
      items.value.push({ kind: 'error', text: payload.message ?? '确认已超时' });
      break;
    }

    case 'error':
      items.value.push({ kind: 'error', text: payload.message ?? '未知错误' });
      running.value = false;
      void refreshSessions();
      break;

    case 'done':
      running.value = false;
      // 一轮结束后会话标题与排序可能都变了（服务端按第一句提问命名）。
      void refreshSessions();
      break;

    default:
      break;
  }
  void scrollToBottom();
}

async function send(): Promise<void> {
  const text = draft.value.trim();
  if (text === '' || running.value || !configured.value) return;

  // 草稿态下先把会话落库，再连它。创建失败就停在这里 ——
  // 否则消息看起来发出去了，实际没有归属的会话可写。
  let sessionId = activeSessionId.value;
  if (sessionId === null) {
    try {
      const created = await api.ai.createSession();
      sessions.value = [created, ...sessions.value];
      activeSessionId.value = created.id;
      sessionId = created.id;
    } catch (error) {
      message.error(describe(error, '创建会话失败'));
      return;
    }
  }

  const ws = connect(sessionId);
  const dispatch = (): void => {
    // 建连是异步的；这期间用户可能已经切走，这条消息不能发到新会话上。
    if (activeSessionId.value !== sessionId) return;
    ws.send(JSON.stringify({ type: 'chat', content: text }));
    items.value.push({ kind: 'user', text });
    draft.value = '';
    running.value = true;
    void scrollToBottom();
  };

  if (ws.readyState === WebSocket.OPEN) {
    dispatch();
    return;
  }
  ws.addEventListener('open', dispatch, { once: true });
}

function stop(): void {
  if (socket?.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ type: 'abort' }));
  }
  running.value = false;
}

function resolveConfirm(item: Extract<Item, { kind: 'confirm' }>, approved: boolean): void {
  if (item.state !== 'pending') return;
  item.state = approved ? 'approved' : 'rejected';
  if (socket?.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ type: 'confirmResult', callId: item.callId, approved }));
  }
}

/**
 * 把服务端保存的历史重建成界面上的消息流。
 *
 * 重建出来的是"当时实际发生的事"，而不是当时界面的样子，两处差异是刻意的：
 *
 *  - 确认卡片退化成普通的工具调用卡片。那次确认早已有了结论，再摆一组
 *    "允许 / 拒绝"按钮只会让人以为还能点。
 *  - 成功与失败取自服务端另记的一份状态（toolStatus），因为模型历史里
 *    （ai.Message）只有 role 和 content，读不出一次命令是成是败。
 *
 * system 消息直接跳过：那是给模型的工作要求，不是这次对话的内容，摆出来
 * 只会让用户以为自己在跟一段配置对话。
 */
function itemsFromHistory(saved: AIChatHistory): Item[] {
  const result: Item[] = [];
  // tool_call_id → 它在 result 里的下标，用来把结果填回对应的卡片。
  const toolIndex = new Map<string, number>();

  for (const msg of saved.history) {
    if (msg.role === 'user') {
      result.push({ kind: 'user', text: msg.content ?? '' });
      continue;
    }

    if (msg.role === 'assistant') {
      // 模型可以边说明边调工具，说明文字和工具卡片要各归各位。
      if (msg.content?.trim()) result.push({ kind: 'assistant', text: msg.content });
      for (const call of msg.tool_calls ?? []) {
        toolIndex.set(call.id, result.length);
        result.push({
          kind: 'tool',
          callId: call.id,
          name: call.function.name,
          args: call.function.arguments,
          // 没有记录的按成功处理：能出现在历史里的调用一定有对应的结果
          // （runToolCall 无论如何都会回一段文本），所以缺记录只可能是存档
          // 来自更早的版本。
          status: saved.toolStatus[call.id] === false ? 'failed' : 'ok',
          output: '',
          expanded: false,
        });
      }
      continue;
    }

    if (msg.role === 'tool') {
      const index = toolIndex.get(msg.tool_call_id ?? '');
      const item = index === undefined ? undefined : result[index];
      if (item?.kind === 'tool') item.output = msg.content ?? '';
    }
  }

  return result;
}

/**
 * 载入服务端保存的对话记录。
 *
 * 失败不弹错误：读不到历史只意味着这一屏是空的，助手本身还能正常用 ——
 * 为它弹一条报错，反而像是在说"助手坏了"。
 */
async function loadHistory(sessionId: number): Promise<void> {
  try {
    const saved = await api.ai.history(sessionId);
    // 请求是异步的：回来时用户可能已经切走，这份历史不能再写进界面。
    if (activeSessionId.value !== sessionId) return;
    items.value = itemsFromHistory(saved);
    void scrollToBottom();
  } catch {
    // 静默：拿不到历史不影响发消息。
  }
}

/** 刷新会话列表（标题与排序）。 */
async function refreshSessions(): Promise<void> {
  try {
    sessions.value = (await api.ai.sessions()).sessions;
  } catch {
    // 侧栏刷新失败不该打断对话；下一次动作会再试。
  }
}

/**
 * 连接被服务端断开后重新对齐会话列表。
 *
 * 正在看的会话可能已经在别处被删掉，此时必须退回"新对话"：继续把消息
 * 发往一个不存在的会话，只会换来一条保存失败的报错。
 */
async function resyncSessions(): Promise<void> {
  await refreshSessions();
  const active = activeSessionId.value;
  if (active === null) return;
  if (sessions.value.some((item) => item.id === active)) return;
  activeSessionId.value = null;
  items.value = [];
}

/**
 * 切换会话。
 *
 * 第一步永远是断开旧连接：服务端的历史、待确认命令与工具状态都挂在连接上，
 * 复用连接等于把上一个会话的内容带进新会话。
 */
async function selectSession(sessionId: number | null): Promise<void> {
  if (sessionId === activeSessionId.value) return;

  disconnect();
  activeSessionId.value = sessionId;
  items.value = [];
  if (sessionId === null) return;
  await loadHistory(sessionId);
}

/** 新建对话：先不落库，等用户发出第一句话时再创建会话。 */
function newSession(): void {
  void selectSession(null);
}

async function removeSession(session: AISession): Promise<void> {
  const deletingActive = session.id === activeSessionId.value;
  // 删的是当前会话就先断开：服务端也会主动断开这条连接，但自己先断能保证
  // 它不会在删除途中再往这个会话里写东西。
  if (deletingActive) disconnect();

  try {
    await api.ai.deleteSession(session.id);
  } catch (error) {
    message.error(describe(error, '删除会话失败'));
    return;
  }

  sessions.value = sessions.value.filter((item) => item.id !== session.id);
  if (!deletingActive) return;

  // 删的是当前会话：落到下一个可用会话，没有就回到"新对话"。
  items.value = [];
  activeSessionId.value = null;
  const next = sessions.value[0];
  if (next) await selectSession(next.id);
}

async function clearChat(): Promise<void> {
  const sessionId = activeSessionId.value;
  if (sessionId === null) {
    items.value = [];
    return;
  }

  try {
    await api.ai.clearHistory(sessionId);
  } catch (error) {
    message.error(describe(error, '清空对话记录失败'));
    return;
  }

  // 断开 WebSocket。服务端手里那份内存历史只在连接存续期间有效，重连时才会
  // 从库里重新读（此时已经是空的）；不断开的话，接着提问仍会带着刚被清掉的
  // 上下文，模型会表现得像没清过一样。
  disconnect();
  items.value = [];
  void refreshSessions();
}

async function initSessions(): Promise<void> {
  try {
    sessions.value = (await api.ai.sessions()).sessions;
  } catch (error) {
    message.error(describe(error, '读取会话列表失败'));
    return;
  }
  // 默认落在最近聊过的会话上；一个都没有时保持草稿态。
  // 用户若在这份列表回来之前就已经发了消息（已经选中了新建的会话），
  // 这里不能再把他切走。
  const first = sessions.value[0];
  if (first && activeSessionId.value === null) await selectSession(first.id);
}

onMounted(() => {
  void loadConfig();
  void initSessions();
});

onBeforeUnmount(() => {
  disconnect();
});
</script>

<template>
  <div class="ai">
    <header class="ai__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">ASSISTANT</p>
        <span class="ai__title page-title">运维助手</span>
        <span class="ai__sub page-meta">
          <template v-if="configured">{{ config.model }} · {{ config.baseUrl }}</template>
          <template v-else>尚未配置模型</template>
        </span>
      </div>
      <span class="ai__spacer" />
      <n-space size="small">
        <!-- 处理中不允许清空：那一轮结束时会把自己的历史写回，清空很快会被覆盖。 -->
        <n-button size="small" :disabled="items.length === 0 || running" @click="clearChat">
          清空对话
        </n-button>
        <n-button size="small" @click="showSettings = true">设置</n-button>
      </n-space>
    </header>

    <div class="ai__body">
      <aside class="ai__sessions">
        <div class="ai__sessions-head">
          <p class="ai__sessions-title">会话</p>
          <span class="ai__sessions-count">{{ sessions.length }}</span>
        </div>
        <button
          class="ai__session ai__session--new"
          :class="{ 'ai__session--active': activeSessionId === null }"
          @click="newSession"
        >
          <span class="ai__session-plus" aria-hidden="true">+</span>
          <span class="ai__session-title">新对话</span>
        </button>
        <div v-for="session in sessions" :key="session.id" class="ai__session-row">
          <button
            class="ai__session"
            :class="{ 'ai__session--active': session.id === activeSessionId }"
            :title="session.title || '新对话'"
            @click="selectSession(session.id)"
          >
            <span class="ai__session-title">{{ session.title || '新对话' }}</span>
          </button>
          <n-popconfirm @positive-click="removeSession(session)">
            <template #trigger>
              <button class="ai__session-del" title="删除会话">×</button>
            </template>
            删除这个会话？对话记录将无法恢复。
          </n-popconfirm>
        </div>
      </aside>

      <div class="ai__main">
        <div ref="scroller" class="ai__stream">
          <div v-if="items.length === 0" class="ai__empty">
            <p class="ai__empty-title">可以直接用自然语言下达运维指令</p>
            <ul class="ai__empty-list">
              <li>帮我看看哪台机器的 CPU 使用率最高</li>
              <li>检查 127.0.0.1 的磁盘占用，找出大于 1G 的日志文件</li>
              <li>看看 nginx 最近有没有报错，重点看 502</li>
            </ul>
            <p v-if="configLoaded && !configured" class="ai__warn">
              还没有配置模型接口。点击右上角「设置」填写接口地址与模型名后即可使用。
            </p>
          </div>

          <template v-for="(item, index) in items" :key="index">
            <div v-if="item.kind === 'user'" class="ai__row ai__row--user">
              <div class="ai__bubble ai__bubble--user">{{ item.text }}</div>
            </div>

            <div v-else-if="item.kind === 'assistant'" class="ai__row">
              <!--
                模型的回复带 markdown（标题、表格、粗体），之前是纯文本直出，
                那些标记就原样露在气泡里。renderMarkdown 生成的是受控 HTML：
                源文本先整体转义，再套白名单标签，所以这里用 v-html 是安全的
                —— 安全来自生成方式，而不是来自事后的过滤。
              -->
              <div class="ai__bubble ai__bubble--md" v-html="renderMarkdown(item.text)"></div>
            </div>

            <div v-else-if="item.kind === 'error'" class="ai__row">
              <div class="ai__bubble ai__bubble--error">{{ item.text }}</div>
            </div>

            <div v-else-if="item.kind === 'tool'" class="ai__tool">
              <header class="ai__tool-head" @click="item.expanded = !item.expanded">
                <n-tag
                  size="tiny"
                  :bordered="false"
                  :type="item.status === 'running' ? 'info' : item.status === 'ok' ? 'success' : 'error'"
                >
                  {{ toolLabel(item.name) }}
                </n-tag>
                <span class="ai__tool-args">{{ formatToolArgs(item.args) }}</span>
                <span class="ai__tool-toggle">
                  {{
                    item.status === 'running' ? '执行中…' : item.expanded ? '收起' : '查看命令与输出'
                  }}
                </span>
              </header>
              <!--
                展开时把完整命令单独列出来。工具栏那一行最多显示两行、到点就截断，
                而用户点开这张卡片想看的多半正是这条命令到底长什么样。
              -->
              <pre v-if="item.expanded && item.args" class="ai__tool-cmd">{{ formatToolArgs(item.args) }}</pre>
              <pre v-if="item.expanded && item.output" class="ai__tool-output">{{ item.output }}</pre>
            </div>

            <div v-else-if="item.kind === 'confirm'" class="ai__confirm">
              <p class="ai__confirm-title">该命令触及安全护栏：{{ item.reason }}</p>
              <pre class="ai__confirm-cmd">{{ item.command }}</pre>
              <div v-if="item.state === 'pending'" class="ai__confirm-actions">
                <n-button size="small" type="primary" @click="resolveConfirm(item, true)">
                  允许执行
                </n-button>
                <n-button size="small" type="error" @click="resolveConfirm(item, false)">拒绝</n-button>
              </div>
              <p v-else class="ai__confirm-state">
                {{
                  item.state === 'approved'
                    ? '已允许执行'
                    : item.state === 'rejected'
                      ? '已拒绝执行'
                      : '确认已超时，本次命令未执行'
                }}
              </p>
            </div>
          </template>
        </div>

        <footer class="ai__composer">
          <n-input
            v-model:value="draft"
            type="textarea"
            :rows="3"
            :disabled="!configLoaded || !configured"
            placeholder="描述你要做的事情，例如：看看哪台机器内存快满了"
            @keydown.enter.exact.prevent="send"
          />
          <div class="ai__composer-actions">
            <span class="ai__hint">Enter 发送，Shift+Enter 换行</span>
            <n-button v-if="running" size="small" type="error" @click="stop">停止</n-button>
            <n-button size="small" type="primary" :disabled="!configured || running" @click="send">
              发送
            </n-button>
          </div>
        </footer>
      </div>
    </div>

    <n-modal v-model:show="showSettings" preset="card" title="模型设置" style="width: 560px">
      <div class="ai__settings">
        <label class="ai__field">
          <span class="ai__field-label">接口地址</span>
          <n-input
            v-model:value="settingsDraft.baseUrl"
            placeholder="https://api.deepseek.com/v1"
          />
        </label>
        <label class="ai__field">
          <span class="ai__field-label">模型名</span>
          <n-input v-model:value="settingsDraft.model" placeholder="deepseek-chat" />
        </label>
        <label class="ai__field">
          <span class="ai__field-label">API Key</span>
          <n-input
            v-model:value="settingsDraft.apiKey"
            type="password"
            show-password-on="click"
            :placeholder="config.hasApiKey ? '已配置，留空表示不修改' : 'sk-...'"
          />
        </label>
        <label class="ai__field">
          <span class="ai__field-label">工具调用轮数上限</span>
          <n-input-number
            v-model:value="settingsDraft.maxSteps"
            :min="1"
            :max="1000"
            :step="10"
            style="width: 100%"
          />
        </label>
        <p class="ai__hint">API Key 会加密后存库，保存后不会再回传到前端。</p>
      </div>

      <template #footer>
        <n-space justify="space-between" style="width: 100%">
          <n-button :loading="testing" @click="testConfig">测试连接</n-button>
          <n-space>
            <n-button @click="showSettings = false">取消</n-button>
            <n-button type="primary" :loading="saving" @click="saveConfig">保存</n-button>
          </n-space>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>
