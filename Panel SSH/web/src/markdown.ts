/**
 * AI 回复的 markdown 渲染。
 *
 * 为什么不引 marked / markdown-it：
 *
 *  1. 它们要么默认放行内嵌 HTML，要么还得再配一个 DOMPurify。两个依赖换来
 *     的是一整套 CommonMark，而模型实际只会用到标题、列表、表格、代码块和
 *     几种行内标记 —— 这个文件覆盖的就是这些。
 *  2. 安全性在这里比完整性重要得多。渲染结果最终会喂给 v-html，而 AI 的
 *     输出是不可信内容。这里的做法是**每一段要输出的文本都先转义、再套
 *     标记规则**，任何 `<script>` 或 `onerror=` 在进入标签之前就变成了
 *     普通文字。安全来自结构，而不是来自事后过滤 —— 事后过滤总有绕过的
 *     方式。
 *
 * 转义刻意放在行内渲染那一层，而不是整个源文本上：块级语法里有 `>` 这种
 * 本身就是 HTML 特殊字符的标记，先整体转义会把引用规则一起废掉。
 *
 * 不支持内嵌 HTML 是刻意的：模型没有理由在回复里写 HTML，而支持它就必须
 * 再引入一整套净化逻辑。
 *
 * 两个地方刻意偏离标准 markdown，因为这里的目标是"让人看清结论"而不是
 * 兼容 CommonMark：
 *
 *  - 段落里的单换行保留成 <br>。模型的换行是有意义的（一行一件事），
 *    按标准折叠掉会把它们挤成一大段。
 *  - 列表不做嵌套层级，任意缩进都当同级。层级判断要引入状态机，而模型
 *    输出的缩进并不严格，猜错了反而更难读。
 */

const escapeMap: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
};

function escapeHtml(text: string): string {
  return text.replace(/[&<>"']/g, (ch) => escapeMap[ch] ?? ch);
}

/**
 * 链接的协议白名单。
 *
 * `[点我](javascript:alert(1))` 在 markdown 里是完全合法的写法 —— 转义
 * HTML 拦不住它，因为危险的是 URL 协议，不是尖括号。只放行这几种，
 * 其余一律保持原样（连链接都不生成）。
 */
function safeHref(raw: string): string {
  const url = raw.trim();
  return /^(https?:\/\/|mailto:)/i.test(url) ? url : '';
}

/**
 * 渲染选项。
 *
 * AI 对话不传任何选项，行为与原来完全一致；文件预览才需要这两项：
 * 代码块着色、以及把相对路径的图片指到远端的预览接口上。
 */
export interface MarkdownRenderOptions {
  /** 代码块着色器；不给则原样转义显示。 */
  highlight?: (code: string, language: string) => string;
  /** 图片地址解析；不给则不渲染图片（保持原来的"当作链接"行为）。 */
  imageUrl?: (src: string) => string | null;
}

/**
 * 渲染一行文本里的行内标记。
 *
 * 转义就发生在这里：传进来的必须是源文本，返回的才是可以进 HTML 的片段。
 * 所有块级分支（标题、列表项、表格单元格、引用、段落）都要经过它，
 * 因此"每个输出点都转过义"这件事只由这一个入口保证。
 */
function renderInline(text: string, options: MarkdownRenderOptions = {}): string {
  let out = escapeHtml(text);

  // 图片只在对图片地址有明确处理器时才渲染：AI 对话里没有"远端文件"这个概念，
  // 保持原来的行为（`![x](y)` 会退化成链接）比凭空多出一条渲染路径更稳。
  if (options.imageUrl) {
    const resolve = options.imageUrl;
    out = out.replace(/!\[([^\]]*)\]\(([^)\s]*)\)/g, (whole, alt: string, src: string) => {
      const url = resolve(src);
      if (!url) return whole;
      return `<img src="${url}" alt="${alt}" loading="lazy" referrerpolicy="no-referrer" />`;
    });
  }

  out = out.replace(/`([^`]+)`/g, (_m, code: string) => `<code>${code}</code>`);
  out = out.replace(/\*\*([^*]+)\*\*/g, (_m, bold: string) => `<strong>${bold}</strong>`);
  // 星号强调放在粗体之后：此时 `**x**` 已经被换成了 <strong>，剩下的单个
  // 星号成对出现才是斜体。
  out = out.replace(/\*([^*\n]+)\*/g, (_m, em: string) => `<em>${em}</em>`);
  out = out.replace(/\[([^\]]*)\]\(([^)\s]*)\)/g, (whole, label: string, href: string) => {
    const url = safeHref(href);
    if (url === '') return whole;
    return `<a href="${url}" target="_blank" rel="noreferrer noopener">${label}</a>`;
  });
  return out;
}

/** 表格行拆成单元格，去掉首尾的竖线。 */
function splitRow(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, '')
    .replace(/\|$/, '')
    .split('|')
    .map((cell) => cell.trim());
}

/**
 * 表格的分隔行，形如 `|---|---|`。
 *
 * 必须要求含竖线：单独一行 `---` 是分隔线（<hr>），把它当成表格分隔行
 * 会让紧随其后的普通文本被渲染成一张单列表格。
 */
function isTableDivider(line: string): boolean {
  const trimmed = line.trim();
  return trimmed.includes('|') && trimmed.includes('-') && /^\|?[\s:|-]+\|?$/.test(trimmed);
}

function renderTable(header: string[], rows: string[][], options: MarkdownRenderOptions = {}): string {
  const head = header.map((cell) => `<th>${renderInline(cell, options)}</th>`).join('');
  const body = rows
    .map((row) => {
      // 行的列数未必和表头一致（模型偶尔漏写），按表头长度补齐，
      // 否则后面的单元格会整体错位。
      const cells = header
        .map((_cell, index) => `<td>${renderInline(row[index] ?? '', options)}</td>`)
        .join('');
      return `<tr>${cells}</tr>`;
    })
    .join('');
  return `<table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table>`;
}

const headingPattern = /^(#{1,6})\s+(.*)$/;
const rulePattern = /^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/;
const bulletPattern = /^\s*[-*+]\s+(.*)$/;
const orderedPattern = /^\s*\d+[.)]\s+(.*)$/;
const quotePattern = /^\s*>\s?/;

/** 这一行会不会开启一个新的块。段落靠它知道该在哪里收尾。 */
function isBlockStart(lines: string[], index: number): boolean {
  const line = lines[index];
  if (/^\s*```/.test(line)) return true;
  if (headingPattern.test(line)) return true;
  if (rulePattern.test(line)) return true;
  if (bulletPattern.test(line)) return true;
  if (orderedPattern.test(line)) return true;
  if (quotePattern.test(line)) return true;
  return line.includes('|') && index + 1 < lines.length && isTableDivider(lines[index + 1]);
}

/** 把一段 markdown 渲染成可以直接交给 v-html 的 HTML。 */
export function renderMarkdown(source: string, options: MarkdownRenderOptions = {}): string {
  const lines = source.split('\n');
  const out: string[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // 围栏代码块：里面的内容一律原样保留，不再套任何标记规则。
    if (/^\s*```/.test(line)) {
      const language = (/^\s*```\s*([^\s`]*)/.exec(line)?.[1] ?? '').trim();
      i += 1;
      const code: string[] = [];
      while (i < lines.length && !/^\s*```/.test(lines[i])) {
        code.push(lines[i]);
        i += 1;
      }
      i += 1; // 跳过收尾的 ```
      const body = code.join('\n');
      // 代码块的内容不经过 renderInline，转义要在这里显式补上 ——
      // 少了这一步，代码块就是一个直通 HTML 的注入点。
      if (options.highlight) {
        const highlighted = options.highlight(body, language);
        const cls = language
          ? `hljs language-${escapeHtml(language)}`
          : 'hljs';
        out.push(`<pre><code class="${cls}">${highlighted}</code></pre>`);
      } else {
        out.push(`<pre><code>${escapeHtml(body)}</code></pre>`);
      }
      continue;
    }

    // 表格要在分隔线之前判断：`|---|---|` 也满足分隔线的形状。
    if (line.includes('|') && i + 1 < lines.length && isTableDivider(lines[i + 1])) {
      const header = splitRow(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].trim() !== '' && lines[i].includes('|')) {
        rows.push(splitRow(lines[i]));
        i += 1;
      }
      out.push(renderTable(header, rows, options));
      continue;
    }

    if (line.trim() === '') {
      i += 1;
      continue;
    }

    const heading = headingPattern.exec(line);
    if (heading) {
      const level = heading[1].length;
      out.push(`<h${level}>${renderInline(heading[2].trim(), options)}</h${level}>`);
      i += 1;
      continue;
    }

    if (rulePattern.test(line)) {
      out.push('<hr />');
      i += 1;
      continue;
    }

    const bullet = bulletPattern.exec(line);
    const ordered = orderedPattern.exec(line);
    if (bullet || ordered) {
      const tag = bullet ? 'ul' : 'ol';
      const pattern = bullet ? bulletPattern : orderedPattern;
      const items: string[] = [];
      while (i < lines.length) {
        const match = pattern.exec(lines[i]);
        if (!match) break;
        items.push(`<li>${renderInline(match[1].trim(), options)}</li>`);
        i += 1;
      }
      out.push(`<${tag}>${items.join('')}</${tag}>`);
      continue;
    }

    if (quotePattern.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && quotePattern.test(lines[i])) {
        quoted.push(lines[i].replace(quotePattern, ''));
        i += 1;
      }
      out.push(`<blockquote>${renderInline(quoted.join('\n'), options)}</blockquote>`);
      continue;
    }

    const paragraph: string[] = [];
    while (i < lines.length && lines[i].trim() !== '' && !isBlockStart(lines, i)) {
      paragraph.push(renderInline(lines[i], options));
      i += 1;
    }
    out.push(`<p>${paragraph.join('<br />')}</p>`);
  }

  return out.join('\n');
}

/**
 * 把工具调用的参数从 JSON 渲染成人能读的一行。
 *
 * 服务端给的是模型原始的工具调用参数，是 JSON 字符串。直接显示有两个毛病：
 * 命令里的引号会带着反斜杠（`\"`）、换行是字面的 `\n`，再叠上工具栏的单行
 * 截断，用户基本看不出这条命令到底要干什么。
 */
export function formatToolArgs(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed === '') return '';

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    // 解析不了就原样显示。它不是 JSON 不代表内容有问题，而猜一个格式
    // 只会显示得更离谱。
    return trimmed;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return trimmed;

  return Object.entries(parsed as Record<string, unknown>)
    .map(([key, value]) => `${key}=${typeof value === 'string' ? value : JSON.stringify(value)}`)
    .join('  ');
}
