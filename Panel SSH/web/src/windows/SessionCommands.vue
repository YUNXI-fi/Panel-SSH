<script setup lang="ts">
/**
 * 终端左侧的命令面板。
 *
 * 与文件侧栏同一个位置、同一套外壳：它们是终端窗口的两块「副屏」，
 * 同时只能出现一个。本组件只决定"有哪些命令、点哪一条"，执行交给
 * TerminalHost —— PTY 句柄在那边，这里拿不到也不该拿。
 */
import { ref } from 'vue';

const emit = defineEmits<{ (e: 'run', command: string): void }>();

/**
 * 内置命令只放观测类。
 *
 * 侧栏离终端只有 240px，误点一条 rm 就来不及了；变更类命令交给用户
 * 自己添加 —— 那是他明确选择要跑的东西。
 */
const BUILTIN = [
  { group: '系统', items: ['uptime', 'df -h', 'free -h', 'uname -a'] },
  {
    group: '进程与端口',
    items: ['ps aux --sort=-%cpu | head -n 15', 'ss -tulnp', 'systemctl --failed'],
  },
  { group: '日志', items: ['journalctl -xe', 'dmesg | tail -n 30'] },
  { group: '容器', items: ['docker ps', 'docker stats --no-stream'] },
];

/** 用户命令存在浏览器本地：跨主机通用，关掉面板也还在。 */
const STORAGE_KEY = 'panelssh.term.commands';

function loadCustom(): string[] {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]');
    return Array.isArray(parsed)
      ? parsed.filter((item): item is string => typeof item === 'string' && item.trim() !== '')
      : [];
  } catch {
    // 存的内容坏了就当作没有：命令列表不值得为它报错。
    return [];
  }
}

const custom = ref<string[]>(loadCustom());
const draft = ref('');

function persist(): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(custom.value));
  } catch {
    // 隐私模式写不进去：本次仍然可用，不打断操作。
  }
}

function add(): void {
  const text = draft.value.trim();
  if (text === '') return;
  draft.value = '';
  // 同一条命令加两次没有意义，还会让侧栏出现两行一模一样的内容。
  if (custom.value.includes(text)) return;
  custom.value = [...custom.value, text];
  persist();
}

function remove(text: string): void {
  custom.value = custom.value.filter((item) => item !== text);
  persist();
}

function run(command: string): void {
  emit('run', command);
}
</script>

<template>
  <aside class="sftp cmdpane" aria-label="常用命令">
    <div class="sftp__head">
      <span class="cmdpane__title">命令</span>
      <span class="sftp__spacer" />
      <span class="cmdpane__hint">点击执行</span>
    </div>

    <div class="cmdpane__body">
      <!--
        每组一张卡片：标题行（组名 + 条数）与一组胶囊命令。
        分组比平铺一长列更好扫 —— 找「日志」时眼睛先落到标题，再只看那一张卡片。
      -->
      <section v-for="group in BUILTIN" :key="group.group" class="cmdpane__group">
        <header class="cmdpane__group-head">
          <p class="cmdpane__group-name">{{ group.group }}</p>
          <span class="cmdpane__count">{{ group.items.length }}</span>
        </header>
        <div class="cmdpane__chips">
          <button
            v-for="item in group.items"
            :key="item"
            class="cmdpane__chip"
            :title="`执行：${item}`"
            @click="run(item)"
          >
            {{ item }}
          </button>
        </div>
      </section>

      <section class="cmdpane__group">
        <header class="cmdpane__group-head">
          <p class="cmdpane__group-name">我的命令</p>
          <span class="cmdpane__count">{{ custom.length }}</span>
        </header>
        <p v-if="custom.length === 0" class="cmdpane__empty">在下方输入命令，回车保存</p>
        <div v-else class="cmdpane__chips">
          <span v-for="item in custom" :key="item" class="cmdpane__mine">
            <button class="cmdpane__chip" :title="`执行：${item}`" @click="run(item)">{{ item }}</button>
            <button
              class="cmdpane__del"
              title="删除这条命令"
              aria-label="删除这条命令"
              @click="remove(item)"
            >
              ×
            </button>
          </span>
        </div>
      </section>
    </div>

    <div class="sftp__foot cmdpane__foot">
      <input
        v-model="draft"
        class="cmdpane__input"
        type="text"
        placeholder="添加命令"
        aria-label="添加命令"
        spellcheck="false"
        @keyup.enter="add"
      />
      <button class="sftp__act" :disabled="draft.trim() === ''" @click="add">添加</button>
    </div>
  </aside>
</template>

<style scoped>
.cmdpane__title {
  color: var(--ink-hi);
  font-size: var(--fs-xs);
}

.cmdpane__hint {
  color: var(--ink-lo);
  font-size: 9px;
}

.cmdpane__body {
  flex: 1;
  min-height: 0;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-2);
}

/*
 * 组卡片：侧栏本身已经是一张卡片，组卡片只换一层抬起面来分组，
 * 不再叠阴影 —— 240px 宽里叠两层投影会让每条命令都像浮在雾里。
 */
.cmdpane__group {
  padding: 10px;
  border: 1px solid var(--line);
  border-radius: var(--r-3);
  background: var(--bg-raise);
}

.cmdpane__group-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--sp-2);
  margin-bottom: 8px;
}

.cmdpane__group-name {
  margin: 0;
  color: var(--sig);
  font-size: 9px;
  font-weight: 600;
  letter-spacing: 0.16em;
}

.cmdpane__count {
  color: var(--ink-lo);
  font-family: var(--font-mono);
  font-size: 9px;
}

/* 命令是短标签而不是列表行：一条命令一个胶囊，靠换行排列，长命令省略号截断。 */
.cmdpane__chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.cmdpane__chip {
  max-width: 100%;
  padding: 3px 9px;
  overflow: hidden;
  border: 1px solid var(--line-strong);
  border-radius: 999px;
  background: transparent;
  color: var(--ink);
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1.5;
  text-align: left;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: pointer;
  transition: border-color var(--dur-1) var(--ease), background var(--dur-1) var(--ease),
    color var(--dur-1) var(--ease);
}

.cmdpane__chip:hover {
  border-color: var(--sig-line);
  background: var(--sig-wash-soft);
  color: var(--ink-hi);
}

/* 自定义命令把删除叉收进同一个胶囊：拆成两个独立按钮会看不出叉属于哪条命令。 */
.cmdpane__mine {
  display: inline-flex;
  align-items: center;
  max-width: 100%;
  border: 1px solid var(--line-strong);
  border-radius: 999px;
  transition: border-color var(--dur-1) var(--ease);
}

.cmdpane__mine:hover {
  border-color: var(--sig-line);
}

.cmdpane__mine .cmdpane__chip {
  min-width: 0;
  border: 0;
  background: transparent;
}

.cmdpane__mine .cmdpane__chip:hover {
  background: transparent;
}

.cmdpane__del {
  flex: none;
  display: grid;
  place-items: center;
  width: 16px;
  height: 16px;
  margin-right: 3px;
  padding: 0;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: var(--ink-lo);
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1;
  cursor: pointer;
}

.cmdpane__del:hover {
  background: var(--danger-wash);
  color: var(--danger-ink);
}

.cmdpane__empty {
  margin: 0;
  color: var(--ink-lo);
  font-size: 9px;
  line-height: 1.6;
}

.cmdpane__foot {
  gap: var(--sp-1);
}

.cmdpane__input {
  flex: 1;
  min-width: 0;
  min-height: 20px;
  padding: 0 var(--sp-2);
  border: 1px solid var(--line-strong);
  border-radius: var(--r-1);
  background: transparent;
  color: var(--ink);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  outline: none;
}

.cmdpane__input:focus {
  border-color: var(--sig);
}

.cmdpane__input::placeholder {
  color: var(--ink-lo);
}
</style>
