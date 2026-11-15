import { createApp } from 'vue';
import naive from 'naive-ui';

import App from './App.vue';
// 令牌必须先于全局样式加载：全局样式全部引用这里的变量，
// 顺序反了会在首帧出现未定义变量的回退色。
import './styles/tokens.css';
import './styles/global.css';
import '@xterm/xterm/css/xterm.css';

// 主题必须在挂载前落到 <html>：否则首帧会先显示默认主题再跳变。
import { initTheme } from './desktop/theme';

initTheme();
createApp(App).use(naive).mount('#app');
