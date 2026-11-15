/*
 * Panel SSH 插件前端 SDK。
 *
 * 插件运行在 sandbox="allow-scripts" 的 iframe 里，其 origin 是 opaque 的：
 *   - 它无法读取父窗口的任何对象（不像同源 iframe 那样能直接取 window.parent.$x）；
 *   - 它的 fetch 会被当作跨源请求，拿不到面板的会话 Cookie。
 * 因此插件与面板之间的唯一通道是 postMessage，所有能力都由父窗口代为执行。
 *
 * 这一条是本 SDK 存在的根本原因：跨源隔离让"插件能不能越权"不再取决于
 * 插件作者的自觉，而取决于父窗口是否放行。
 */
(function () {
  'use strict';

  var script = document.currentScript;
  var panelOrigin = '';
  try {
    panelOrigin = script && script.src ? new URL(script.src).origin : window.location.origin;
  } catch (error) {
    // 拿不到面板地址时退化为同源，postMessage 会被浏览器拒绝，属于明确失败。
    panelOrigin = window.location.origin;
  }

  var pending = new Map();
  var sequence = 0;
  var context = null;

  var readyResolve;
  var ready = new Promise(function (resolve) {
    readyResolve = resolve;
  });

  function call(method, params) {
    return new Promise(function (resolve, reject) {
      if (!panelOrigin) {
        reject(new Error('无法确定面板地址，插件未正确加载 SDK'));
        return;
      }
      var id = ++sequence;
      pending.set(id, { resolve: resolve, reject: reject });
      window.parent.postMessage(
        { source: 'panel-plugin', id: id, method: method, params: params || {} },
        panelOrigin,
      );
    });
  }

  window.addEventListener('message', function (event) {
    // 父窗口有真实 origin，因此这一侧可以严格校验来源。
    if (panelOrigin && event.origin !== panelOrigin) return;

    var data = event.data;
    if (!data || data.source !== 'panel-host') return;

    if (data.type === 'ready') {
      context = data.context || {};
      readyResolve(context);
      return;
    }

    if (typeof data.id !== 'number') return;
    var entry = pending.get(data.id);
    if (!entry) return;
    pending.delete(data.id);

    if (data.ok) {
      entry.resolve(data.result);
    } else {
      entry.reject(new Error(data.error || '调用失败'));
    }
  });

  var api = {
    ready: ready,

    /** 返回握手时下发的上下文：插件标识、已授予能力等。 */
    getContext: function () {
      return context;
    },

    /** 通用调用入口，便于 SDK 未封装新方法时先行接入。 */
    call: call,

    host: {
      list: function () {
        return call('host.list');
      },
      exec: function (hostId, command, options) {
        return call('host.exec', {
          hostId: hostId,
          command: command,
          timeoutSec: (options && options.timeoutSec) || 0,
        });
      },
      readFile: function (hostId, path) {
        return call('host.readFile', { hostId: hostId, path: path });
      },
      writeFile: function (hostId, path, content) {
        return call('host.writeFile', { hostId: hostId, path: path, content: content });
      },
    },

    /**
     * 调用插件自己的后端进程。
     *
     * 方法名与参数由插件自己定义，面板只负责转发。插件进程要操作目标机时，
     * 由进程侧回调面板的能力接口（POST /api/plugin-api/<method>，带令牌），
     * 面板逐次核对它当前被授予的能力 —— 进程模型给插件「算力」，不给插件「权限」。
     */
    rpc: function (method, params) {
      return call('panel.rpc', { method: method, params: params || {} });
    },

    ui: {
      message: function (text, type) {
        return call('ui.message', { text: text, type: type || 'info' });
      },
    },
  };

  window.$panel = api;

  // 主动握手，避免父窗口的 ready 早于本脚本注册监听而丢失。
  try {
    window.parent.postMessage({ source: 'panel-plugin', type: 'hello' }, panelOrigin);
  } catch (error) {
    console.error('[panel-sdk] 握手失败', error);
  }
})();
