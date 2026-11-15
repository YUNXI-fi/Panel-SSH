<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { useMessage } from 'naive-ui';

import { ApiError, api, DSH_WORKSPACE_NAME, DSH_WORKSPACE_ORG, pluginEntryURL, type PluginInfo } from '../api/client';
import DshManager from './DshManager.vue';

const props = defineProps<{ plugin: PluginInfo; variant?: 'app' | 'config' }>();

const isEmbedded = computed(() => props.plugin.source === 'embedded');

/**
 * 是不是 DSH 工作台。
 *
 * 只有它替换掉了"整窗一层 iframe"的形态：管理器要展示安装形态、运行控制、
 * 工作区与插件，这些都不是 dsh 自己页面里的东西。
 */
const isDshWorkspace = computed(
  () =>
    props.plugin.source === 'embedded' &&
    props.plugin.org === DSH_WORKSPACE_ORG &&
    props.plugin.name === DSH_WORKSPACE_NAME,
);

/**
 * 是否渲染管理器。
 *
 * 设置变体仍走 dsh 自己的 config=1 状态页 —— 那是"启动命令填错、工作台打不开"
 * 时的兜底入口，不能被管理器顶掉。
 */
const showManager = computed(() => isDshWorkspace.value && props.variant !== 'config');

/**
 * 内置插件的入口地址。
 *
 * config 变体走 config=1：内置插件跑起来后，它的入口路径会被自身的界面接管，
 * 只有带这个参数才能切回设置页。第三方插件没有这个概念，原样返回。
 */
const frameSrc = computed(() => {
  const base = pluginEntryURL(props.plugin);
  if (!isEmbedded.value || props.variant !== 'config') return base;
  return `${base}${base.includes('?') ? '&' : '?'}config=1`;
});

const message = useMessage();
const frame = ref<HTMLIFrameElement | null>(null);

interface BridgeMessage {
  source?: string;
  type?: string;
  id?: number;
  method?: string;
  params?: Record<string, unknown>;
}

/**
 * 沙箱 iframe 的 origin 是 opaque（值为 "null"），无法为它指定精确的 targetOrigin；
 * 而收件人是由 event.source 这个具体 window 对象决定的，其它窗口拿不到这份消息，
 * 所以此处用 '*' 不会造成信息泄露。真正的把关在接收侧（见 onMessage）。
 */
function post(target: Window, payload: Record<string, unknown>): void {
  target.postMessage({ source: 'panel-host', ...payload }, '*');
}

/**
 * 握手时下发给插件的上下文。
 *
 * 这里必须产出「纯数据」：postMessage 走结构化克隆，而 Vue 的响应式对象是
 * Proxy，克隆会直接抛 DataCloneError，握手会静默失败（插件一直卡在 ready）。
 * 因此 capabilities 用展开运算符转成普通数组，其余字段逐个取值。
 */
function context(): Record<string, unknown> {
  return {
    plugin: {
      id: `${props.plugin.org}/${props.plugin.name}`,
      displayName: props.plugin.displayName,
      version: props.plugin.version,
    },
    capabilities: [...props.plugin.granted],
  };
}

/** 能力检查在父窗口侧执行：插件无法绕过，因为它没有别的通道。 */
function requireCapability(capability: string): void {
  if (!props.plugin.granted.includes(capability)) {
    throw new Error(`插件未获授权：${capability}`);
  }
}

/**
 * 校验插件要操作的主机。
 *
 * 插件的授权范围不是"所有主机"，而是"它被装上去的那一台"：一个插件之所以
 * 有 host:exec，是因为管理员在某一台机器上把它装了、并且为这台机器授了权。
 * 因此 hostId 由插件自报时必须与该插件的 hostId 完全一致，否则一个装在
 * 低权限机器上的插件就能借这个通道去操作全部主机 —— 这是越权，不是功能。
 *
 * 注意不能用 `!hostId` 之类做存在性判断：本机（宿主机）的 hostId 就是 0，
 * 这类判断会把"本机"当成"没填"而拒绝掉。
 */
function requireOwnHost(raw: unknown): number {
  const hostId = Number(raw);
  if (!Number.isInteger(hostId) || hostId !== props.plugin.hostId) {
    throw new Error('插件只能操作自己被安装到的主机');
  }
  return hostId;
}

async function dispatch(method: string, params: Record<string, unknown>): Promise<unknown> {
  switch (method) {
    case 'panel.context':
      return context();

    case 'host.list': {
      requireCapability('host:list');
      // 只回自己那一台，不回全部主机：清单本身就是越权的另一半 ——
      // 拿到全部主机等于给了插件一份"还能去哪些机器"的靶子列表。
      const hosts = await api.hosts.list();
      return hosts
        .filter((host) => host.id === props.plugin.hostId)
        .map((host) => ({
          id: host.id,
          name: host.name,
          addr: host.addr,
          port: host.port,
          username: host.username,
          group: host.group,
        }));
    }

    case 'host.exec': {
      requireCapability('host:exec');
      const hostId = requireOwnHost(params.hostId);
      const command = String(params.command ?? '');
      if (command.trim() === '') {
        throw new Error('command 为必填');
      }
      return api.hosts.exec(hostId, command, Number(params.timeoutSec ?? 0));
    }

    case 'host.readFile': {
      requireCapability('host:file:read');
      const file = await api.files.readText(
        requireOwnHost(params.hostId),
        String(params.path ?? ''),
      );
      return { path: file.path, content: file.content, size: file.size };
    }

    case 'host.writeFile': {
      requireCapability('host:file:write');
      await api.files.writeText(
        requireOwnHost(params.hostId),
        String(params.path ?? ''),
        String(params.content ?? ''),
      );
      return { ok: true };
    }

    case 'ui.message': {
      const text = String(params.text ?? '');
      const type = String(params.type ?? 'info');
      if (type === 'error') message.error(text);
      else if (type === 'success') message.success(text);
      else if (type === 'warning') message.warning(text);
      else message.info(text);
      return { ok: true };
    }

    case 'panel.rpc': {
      // 转发给插件自己的后端进程。这里不检查能力：进程能做什么，由它在回调
      // 面板时按当前授权逐项判定（见 internal/api/plugin_api.go），
      // 前端这一段只是把请求送进进程，它本身不构成一次越权。
      const method = String(params.method ?? '');
      if (method.trim() === '') {
        throw new Error('method 为必填');
      }
      const res = await api.plugins.rpc(
        props.plugin.org,
        props.plugin.name,
        method,
        props.plugin.hostId,
        params.params ?? {},
      );
      return res.result;
    }

    default:
      throw new Error(`不支持的方法：${method}`);
  }
}

function onMessage(event: MessageEvent): void {
  // 沙箱文档的 origin 是 'null'，无法靠 origin 区分不同插件；
  // 因此身份判定改用 event.source 与 iframe.contentWindow 的恒等比对 ——
  // 这个引用由浏览器给出，其它窗口伪造不了。
  if (event.source !== frame.value?.contentWindow) return;

  const data = event.data as BridgeMessage | null;
  if (!data || data.source !== 'panel-plugin') return;

  const target = event.source as Window;

  if (data.type === 'hello') {
    post(target, { type: 'ready', context: context() });
    return;
  }

  if (typeof data.id !== 'number' || typeof data.method !== 'string') return;

  const { id, method } = data;
  dispatch(method, data.params ?? {})
    .then((result) => post(target, { id, ok: true, result }))
    .catch((error: unknown) => {
      const text =
        error instanceof ApiError
          ? error.message
          : error instanceof Error
            ? error.message
            : String(error);
      post(target, { id, ok: false, error: text });
    });
}

onMounted(() => {
  if (isEmbedded.value) return;
  window.addEventListener('message', onMessage);
});

onBeforeUnmount(() => window.removeEventListener('message', onMessage));
</script>

<template>
  <div class="plugin">
    <DshManager v-if="showManager" :plugin="plugin" />

    <template v-else>
      <header class="plugin__bar">
        <span class="plugin__name">{{ plugin.displayName }}</span>
        <span class="plugin__meta">{{ plugin.org }}/{{ plugin.name }} · v{{ plugin.version }}</span>
        <span class="plugin__spacer" />
        <span class="plugin__meta">
          {{
            isEmbedded
              ? variant === 'config'
                ? '内置插件 · 设置'
                : '内置插件 · 以面板自身身份运行'
              : `已授权 ${plugin.granted.length} / ${plugin.capabilities.length} 项能力`
          }}
        </span>
      </header>

      <!--
        第三方插件：sandbox 只给 allow-scripts，不给 allow-same-origin，
        插件因此处于 opaque origin，读不到父窗口对象、带不上会话 Cookie，
        所有敏感操作只能经能力桥由主应用代办。
      -->
      <iframe
        v-if="!isEmbedded"
        ref="frame"
        class="plugin__frame"
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
        :src="frameSrc"
        :title="plugin.displayName"
      />

      <!-- 内置插件：第一方代码，与面板同源直接加载，不加 sandbox。 -->
      <iframe
        v-else
        class="plugin__frame"
        referrerpolicy="no-referrer"
        :src="frameSrc"
        :title="plugin.displayName"
      />
    </template>
  </div>
</template>
