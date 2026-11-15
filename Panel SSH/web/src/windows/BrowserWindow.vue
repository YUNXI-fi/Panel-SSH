<script setup lang="ts">
/**
 * 浏览器窗口：借一台已有主机访问网页。
 *
 * 一个窗口里两条通路，取舍恰好相反，因此都摆在这里：
 *
 *   - 内嵌浏览：窗口里直接输网址。面板把页面经目标机取回来、改写成本地镜像后
 *     放进 iframe，零配置；代价是内容必须被改写，只有结构简单的内网页面可靠。
 *   - 系统代理：面板本机开一个 SOCKS5 端口，用户填进自己的浏览器。要手工配一次，
 *     但页面从源站原样加载，什么网站都能开，域名也在目标机上解析。
 *
 * 为什么不把内嵌浏览做成唯一实现：面板跑在用户的浏览器里，窗口内的页面只能是
 * iframe，而 iframe 的网络请求走用户本机的网络栈，面板后端插不进手 —— 想让它
 * "经由主机"，就只能由服务端取回页面再改写后转发，而现代站点（CSP、SRI、
 * JS 拼装地址）改写不过来。真正"什么站都能开"的那条路是系统代理。
 *
 * iframe 是沙箱化的、且刻意不给 allow-same-origin：被代理页面与面板同处一个域，
 * 若不隔开，页面里的脚本就能读到面板的 DOM、用面板的会话调面板的接口。
 * 隔开之后它的源是 opaque，那些路全部走不通；代价是页面读不到自己的
 * document.cookie 与本地存储，重前端的站点因此更容易显示不全。
 *
 * 但沙箱还带来一个后果：浏览器只给 opaque origin 带 SameSite=None 的 Cookie，
 * 而面板的会话 Cookie 是 SameSite=Lax —— 也就是说镜像路径根本收不到会话。
 * 所以镜像用的是另一套凭据：开窗时向面板领一枚"只能读网页"的短时令牌，
 * 放进镜像路径里（相对链接会自动带着它）。令牌换不来会话，页面即使被
 * 恶意脚本控制，也只能读网页，出不去面板这一层。
 */
import { computed, onMounted, onUnmounted, ref, watch } from 'vue';
import { NAlert, NButton, NInput, NRadioButton, NRadioGroup, NTag, useMessage } from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import { LOCAL_HOST_ID } from '../desktop/watchedHost';
import { ApiError, api, copyText, type BrowserMirrorToken, type BrowserProxyStatus } from '../api/client';

const props = defineProps<{ hostId?: number; hostName?: string }>();

const message = useMessage();

/**
 * 窗口自己持有目标主机；从主机管理带 hostId 打开时就是它。
 *
 * 选择器里包含宿主机，这与系统代理那条路不同：取网页的是面板自己，
 * 因此哪怕浏览器在另一台机器上，也能借面板本机的网络（VPN、专线）访问。
 * 而 SOCKS 端口只监听回环，只有面板本机上的浏览器能用它 ——
 * 那种情况下"借本机出网"等于绕一圈回到原地，所以服务端会明确挡下。
 */
const target = ref<number | null>(props.hostId ?? null);
const targetName = ref(props.hostName ?? '');

const mode = ref<'page' | 'socks'>('page');

/**
 * 地址栏显示的是目标站的真实地址，不是镜像路径。
 *
 * 镜像路径（/browser/1/http/10.0.0.5:8080/admin）是实现细节，对用户没有意义，
 * 因此只在这里内部换算：进 iframe 时转成镜像，页内跳转由 shim 回报真实地址。
 */
const address = ref('');
const current = ref('');
const frameSrc = ref('');
const loading = ref(false);
const history = ref<string[]>([]);
const cursor = ref(-1);
const pageError = ref('');

/**
 * 当前主机的镜像凭据。与主机绑定，换主机就得重新领。
 *
 * 缓存到快过期为止：每开一个地址都重新签发没有意义，而令牌本身有期限，
 * 到期后 iframe 里只会看到一句"凭据已失效"，所以提前一分钟换新的。
 */
const mirror = ref<BrowserMirrorToken | null>(null);

const frame = ref<HTMLIFrameElement | null>(null);

// 宿主机是合法目标：直连即可，服务端不需要 SSH 通道。
const isLocalHost = computed(() => target.value === LOCAL_HOST_ID);

/**
 * 取一枚可用的镜像凭据，拿不到时返回空串并留下提示。
 *
 * 镜像路径不能用会话 Cookie 鉴权（沙箱 iframe 收不到 SameSite=Lax 的 Cookie），
 * 因此每次开窗、换主机、以及凭据临近过期时都要来这里领一枚。
 */
async function ensureToken(): Promise<string> {
  const id = target.value;
  if (id === null) return '';

  const cached = mirror.value;
  if (cached && cached.expiresAt - Date.now() > 60_000) return cached.token;

  try {
    mirror.value = await api.browser.mirrorToken(id);
    pageError.value = '';
    return mirror.value.token;
  } catch (err) {
    pageError.value = err instanceof ApiError ? err.message : '取镜像凭据失败，请重试';
    return '';
  }
}

/**
 * 把用户输入补成一个完整地址。
 *
 * 内网里最常见的写法是 host:port 甚至是裸 IP，这些在 new URL 里会被当成
 * 协议名（"10.0.0.5:" 看着就像一个 scheme），因此没有 "://" 时一律补上 http://。
 */
function normalizeURL(raw: string): string | null {
  const text = raw.trim();
  if (!text) return null;

  const candidate = /^[a-z][a-z0-9+.-]*:\/\//i.test(text) ? text : `http://${text}`;
  try {
    const url = new URL(candidate);
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
    return url.toString();
  } catch {
    return null;
  }
}

/** 把目标地址换算成本机镜像路径。规则见 internal/api/browser.go 的 handleBrowserProxy。 */
function mirrorPath(url: string, token: string): string {
  const id = target.value;
  if (id === null) return '';

  const parsed = new URL(url);
  return (
    `/browser/${id}/${token}/${parsed.protocol.replace(':', '')}/${parsed.host}` +
    parsed.pathname +
    parsed.search +
    parsed.hash
  );
}

/** 用户主动发起的一次跳转：地址栏、页内后退/前进、换主机都走这里。 */
async function goTo(url: string, push = true): Promise<void> {
  const token = await ensureToken();
  if (!token) return;

  current.value = url;
  address.value = url;
  loading.value = true;
  frameSrc.value = mirrorPath(url, token);

  if (!push) return;
  history.value = history.value.slice(0, cursor.value + 1);
  history.value.push(url);
  cursor.value = history.value.length - 1;
}

async function openAddress(): Promise<void> {
  if (target.value === null) return;

  const url = normalizeURL(address.value);
  if (!url) {
    message.error('这不像一个网址，请填成 10.0.0.5:8080 或 http://10.0.0.5:8080 这样的形式');
    return;
  }
  await goTo(url);
}

/** 页内跳转：页面已经自己走过去了，这里只同步地址栏与历史，不再让它重载一次。 */
function trackInPage(url: string): void {
  current.value = url;
  address.value = url;
  history.value = history.value.slice(0, cursor.value + 1);
  history.value.push(url);
  cursor.value = history.value.length - 1;
}

const canGoBack = computed(() => cursor.value > 0);
const canGoForward = computed(() => cursor.value >= 0 && cursor.value < history.value.length - 1);

function go(delta: number): void {
  const next = cursor.value + delta;
  if (next < 0 || next >= history.value.length) return;

  cursor.value = next;
  void goTo(history.value[next], false);
}

/**
 * 接收页内回报的地址。
 *
 * iframe 是沙箱化的，外层读不到它的 location，页内跳转（点链接、脚本改地址）
 * 只能由注入的 shim 主动报上来 —— 否则地址栏会一直停在用户上次手填的那一串。
 */
function onFrameMessage(event: MessageEvent): void {
  if (!frame.value || event.source !== frame.value.contentWindow) return;

  const data = event.data as { panelsshBrowser?: number; url?: string } | null;
  if (!data || data.panelsshBrowser !== 1 || typeof data.url !== 'string') return;

  loading.value = false;
  if (data.url === current.value) return;
  trackInPage(data.url);
}

onMounted(() => window.addEventListener('message', onFrameMessage));
onUnmounted(() => window.removeEventListener('message', onFrameMessage));

// ---------------- 系统代理 ----------------

const status = ref<BrowserProxyStatus>({ running: false, addr: '' });
const busy = ref(false);
const error = ref('');

/**
 * 代理地址与端口。
 *
 * 服务端只给 host:port：SOCKS5 这个前缀是"填进浏览器时要写的形态"，
 * 属于界面的事，放在这里拼。
 */
const proxyURL = computed(() => (status.value.addr ? `socks5://${status.value.addr}` : ''));
const proxyHost = computed(() => (status.value.addr ? status.value.addr.slice(0, status.value.addr.lastIndexOf(':')) : ''));
const proxyPort = computed(() => (status.value.addr ? status.value.addr.slice(status.value.addr.lastIndexOf(':') + 1) : ''));

async function refreshProxy(): Promise<void> {
  const id = target.value;
  if (id === null) {
    status.value = { running: false, addr: '' };
    return;
  }
  busy.value = true;
  try {
    status.value = await api.browser.socksStatus(id);
    error.value = '';
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '读取代理状态失败';
  } finally {
    busy.value = false;
  }
}

/**
 * 启停代理。
 *
 * 换主机时不会顺手停掉上一台的代理 —— 那台机器上的代理是它自己的资源，
 * 用户可能还要用（同时盯两台机器正是这个应用允许多开窗口的理由）。
 * 因此窗口里明确写着一句"代理不随窗口关闭而停止"。
 */
async function toggleProxy(start: boolean): Promise<void> {
  const id = target.value;
  if (id === null) return;

  busy.value = true;
  error.value = '';
  try {
    status.value = start ? await api.browser.startSocks(id) : await api.browser.stopSocks(id);
    message.success(start ? `代理已启动：${proxyURL.value}` : '代理已停止');
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '操作失败';
  } finally {
    busy.value = false;
  }
}

async function copyProxy(): Promise<void> {
  if (!proxyURL.value) return;

  if (await copyText(proxyURL.value)) {
    message.success('已复制代理地址');
    return;
  }
  // 复制失败不吞掉：地址就在眼前，让用户知道得手动选一次，而不是以为已经复制好了。
  message.error('复制失败，请手动选中地址复制');
}

// 换主机要做三件事：镜像凭据与主机绑定，必须作废重领；代理状态要重新问一次
// （只问这一次，不做轮询 —— 它只在按按钮时变）；当前页面要经新主机重新取一遍。
// 同一个网址换台机器看，正是这个功能的常用玩法。
watch(target, () => {
  mirror.value = null;
  void refreshProxy();
  if (current.value) void goTo(current.value, false);
}, { immediate: true });
</script>

<template>
  <div class="browser">
    <div class="browser__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">BROWSER</p>
        <h2 class="page-title">网页浏览</h2>
      </div>
      <n-radio-group v-model:value="mode" size="small">
        <n-radio-button value="page">内嵌浏览</n-radio-button>
        <n-radio-button value="socks">系统代理</n-radio-button>
      </n-radio-group>

      <span class="browser__spacer" />

      <n-tag
        v-if="mode === 'socks'"
        :type="status.running ? 'success' : 'default'"
        size="small"
        :bordered="false"
      >
        {{ status.running ? '代理运行中' : '未启动' }}
      </n-tag>
      <n-button v-if="mode === 'socks'" size="small" :loading="busy" @click="refreshProxy">
        刷新
      </n-button>

      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <HostPicker v-model:host-id="target" v-model:host-name="targetName" include-local />
    </div>

    <n-alert v-if="target === null" type="info" :bordered="false">
      先选择一台主机 —— 它的网络就是网页的出网路径。
    </n-alert>

    <!-- 内嵌浏览 -->
    <template v-else-if="mode === 'page'">
      <div class="browser__nav">
        <n-button size="small" :disabled="!canGoBack" @click="go(-1)">←</n-button>
        <n-button size="small" :disabled="!canGoForward" @click="go(1)">→</n-button>
        <n-input
          v-model:value="address"
          size="small"
          placeholder="10.0.0.5:8080 或 http://10.0.0.5:8080/admin"
          aria-label="要访问的地址"
          @keyup.enter="openAddress"
        />
        <n-button size="small" type="primary" @click="openAddress">打开</n-button>
        <n-button size="small" :disabled="!current" @click="goTo(current, false)">刷新</n-button>
      </div>

      <n-alert v-if="pageError" type="error" :bordered="false">{{ pageError }}</n-alert>

      <div class="browser__view">
        <iframe
          v-if="frameSrc"
          ref="frame"
          class="browser__frame"
          :src="frameSrc"
          sandbox="allow-scripts allow-forms allow-popups"
          @load="loading = false"
        ></iframe>
        <div v-else class="browser__blank">
          <p class="browser__blank-title">在上面填一个地址</p>
          <p>
            网页会经<b>{{ targetName || '所选主机' }}</b>取回来。适合看内网里的管理页、路由器、
            接口文档这类结构简单的页面。
          </p>
          <p class="browser__blank-hint">
            重前端的站点（依赖 CSP / 单页应用 / 登录态存储）多半显示不全 ——
            被代理的页面跑在隔离沙箱里，读不到自己的 Cookie 与本地存储。
            那种情况请切到「系统代理」，把你自己的浏览器指过去 —— 页面从源站原样加载，什么站都能开。
          </p>
        </div>
        <div v-if="loading && frameSrc" class="browser__loading">正在取页面…</div>
      </div>
    </template>

    <!-- 系统代理 -->
    <div v-else class="browser__scroll">
      <n-alert v-if="isLocalHost" type="info" :bordered="false">
        宿主机就是面板本机，而这个代理只监听回环地址、只有面板本机上的浏览器能连上它 ——
        经本机出网等于绕一圈回到原地，因此这里不提供宿主机的代理。
        想借本机网络看网页，请切到「内嵌浏览」。
      </n-alert>

      <template v-else>
        <n-alert v-if="error" type="error" :bordered="false">{{ error }}</n-alert>

        <section class="browser__card">
          <div class="browser__head">
            <span class="silk">系统代理</span>
            <span class="browser__note">把面板本机的一个端口变成「从这台主机出网」的入口</span>
          </div>

          <div class="browser__row">
            <span class="silk browser__label">代理地址</span>
            <code class="browser__addr" :class="{ 'is-idle': !status.running }">
              {{ proxyURL || '启动后在这里显示' }}
            </code>
            <n-button size="small" :disabled="!status.running" @click="copyProxy">复制</n-button>
            <n-button v-if="status.running" size="small" :loading="busy" @click="toggleProxy(false)">
              停止代理
            </n-button>
            <n-button v-else size="small" type="primary" :loading="busy" @click="toggleProxy(true)">
              启动代理
            </n-button>
          </div>

          <ul class="browser__facts">
            <li>
              代理只监听回环地址，因此只有<b>面板所在这台机器</b>上的浏览器能连上它；
              换台电脑访问面板时，需要在那边另想办法（面板不对外暴露这个端口）。
            </li>
            <li>
              请求在目标机上解析域名，所以那台机器能解析的内网域名同样可以打开 ——
              这正是"借它的网络"最直接的用处。
            </li>
            <li>
              代理不会随窗口关闭而停止。不用时请点「停止代理」：端口开着期间，
              这台机器上的任何程序都可以借它出网。
            </li>
          </ul>
        </section>

        <section class="browser__card">
          <div class="browser__head">
            <span class="silk">怎么配</span>
            <span class="browser__note">配一次就行，之后切换主机只需换端口</span>
          </div>

          <dl class="browser__howto">
            <dt>Firefox</dt>
            <dd>
              设置 → 网络设置 → 手动配置代理 → SOCKS 主机
              <code>{{ proxyHost || '127.0.0.1' }}</code>、端口
              <code>{{ proxyPort || '（启动后显示）' }}</code>，选中 SOCKS v5。
              Firefox 是这里最省事的一个：它的代理设置独立于系统，不影响其他程序。
            </dd>

            <dt>Chrome / Edge</dt>
            <dd>
              用启动参数
              <code>--proxy-server="{{ proxyURL || 'socks5://127.0.0.1:端口' }}"</code>
              打开一个新的浏览器实例（建议另建一个快捷方式）：
              这两个浏览器的代理只能通过启动参数或扩展指定，而启动参数不影响已在运行的窗口。
            </dd>

            <dt>系统代理</dt>
            <dd>
              多数 Linux 桌面可在「网络设置 → 代理」里填 SOCKS 主机与端口，
              那样这台机器上所有走系统代理的程序都会从目标机出网。
            </dd>
          </dl>
        </section>
      </template>
    </div>
  </div>
</template>