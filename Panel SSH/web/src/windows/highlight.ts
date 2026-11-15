/**
 * 代码高亮。
 *
 * 用 highlight.js 的 core + 手动注册语言，而不是整包：整包含近 200 种语言，
 * 首屏会多出好几百 KB，而面板里真正会打开的就是这些配置与源码文件。
 *
 * 不做自动识别（highlightAuto）：它会把每种语言都试一遍，大文件直接卡死，
 * 而且对短片段猜得很不准 —— 认不出语言时按纯文本显示，颜色少一点好过颜色错乱。
 */

import hljs from 'highlight.js/lib/core';

import bash from 'highlight.js/lib/languages/bash';
import c from 'highlight.js/lib/languages/c';
import cpp from 'highlight.js/lib/languages/cpp';
import csharp from 'highlight.js/lib/languages/csharp';
import css from 'highlight.js/lib/languages/css';
import dockerfile from 'highlight.js/lib/languages/dockerfile';
import go from 'highlight.js/lib/languages/go';
import ini from 'highlight.js/lib/languages/ini';
import java from 'highlight.js/lib/languages/java';
import javascript from 'highlight.js/lib/languages/javascript';
import json from 'highlight.js/lib/languages/json';
import makefile from 'highlight.js/lib/languages/makefile';
import markdown from 'highlight.js/lib/languages/markdown';
import nginx from 'highlight.js/lib/languages/nginx';
import php from 'highlight.js/lib/languages/php';
import powershell from 'highlight.js/lib/languages/powershell';
import python from 'highlight.js/lib/languages/python';
import ruby from 'highlight.js/lib/languages/ruby';
import rust from 'highlight.js/lib/languages/rust';
import sql from 'highlight.js/lib/languages/sql';
import typescript from 'highlight.js/lib/languages/typescript';
import xml from 'highlight.js/lib/languages/xml';
import yaml from 'highlight.js/lib/languages/yaml';

import { MAX_HIGHLIGHT_CHARS } from './filePreview';

hljs.registerLanguage('bash', bash);
hljs.registerLanguage('c', c);
hljs.registerLanguage('cpp', cpp);
hljs.registerLanguage('csharp', csharp);
hljs.registerLanguage('css', css);
hljs.registerLanguage('dockerfile', dockerfile);
hljs.registerLanguage('go', go);
hljs.registerLanguage('ini', ini);
hljs.registerLanguage('java', java);
hljs.registerLanguage('javascript', javascript);
hljs.registerLanguage('json', json);
hljs.registerLanguage('makefile', makefile);
hljs.registerLanguage('markdown', markdown);
hljs.registerLanguage('nginx', nginx);
hljs.registerLanguage('php', php);
hljs.registerLanguage('powershell', powershell);
hljs.registerLanguage('python', python);
hljs.registerLanguage('ruby', ruby);
hljs.registerLanguage('rust', rust);
hljs.registerLanguage('sql', sql);
hljs.registerLanguage('typescript', typescript);
hljs.registerLanguage('xml', xml);
hljs.registerLanguage('yaml', yaml);

const escapeMap: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
};

/** 把纯文本转成可以放进 innerHTML 的形式。 */
export function escapePlain(text: string): string {
  return text.replace(/[&<>"']/g, (ch) => escapeMap[ch] ?? ch);
}

/**
 * 高亮一段代码，返回可直接交给 v-html 的 HTML。
 *
 * highlight.js 自己会转义源码，所以这里的输出是安全的；认不出语言、超出体积
 * 上限或语言模块抛错时退回纯文本（同样先转义）。
 */
export function highlightCode(code: string, language: string | null): string {
  if (code.length > MAX_HIGHLIGHT_CHARS) return escapePlain(code);
  if (!language || !hljs.getLanguage(language)) return escapePlain(code);
  try {
    return hljs.highlight(code, { language, ignoreIllegals: true }).value;
  } catch {
    return escapePlain(code);
  }
}
